package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/enetforge/hogshead/internal/cellar"
	"github.com/enetforge/hogshead/internal/usage"
)

// ErrorEntry is one failed request.
type ErrorEntry struct {
	usage.Record
	Member string `json:"member,omitempty"`
}

// ErrorGroup summarises failed requests that share a cause.
type ErrorGroup struct {
	Code     string    `json:"code"`
	Status   int       `json:"status"`
	Op       string    `json:"op"`
	Bucket   string    `json:"bucket,omitempty"`
	User     string    `json:"user,omitempty"`
	Count    int       `json:"count"`
	LastSeen time.Time `json:"last_seen"`
	Message  string    `json:"message,omitempty"` // the server's message from the latest occurrence
	Hint     string    `json:"hint"`              // what it usually means and whether to worry
	Severity string    `json:"severity"`          // info | warning | error
	Members  []string  `json:"members,omitempty"`
}

// ErrorsResponse answers GET /errors.
type ErrorsResponse struct {
	From    time.Time     `json:"from"`
	Scope   string        `json:"scope"`
	Total   int           `json:"total"`
	Groups  []ErrorGroup  `json:"groups"`
	Rows    []ErrorEntry  `json:"rows"`
	Members []UsageMember `json:"members,omitempty"`
}

func (h *Handler) errorRoutes() []Route {
	return []Route{
		{"GET", "/errors", "Recent failed S3 requests with error code, message and an explanation, grouped by cause (?from=&limit=&bucket=&user=&code=&status=&scope=local|cellar)", true, h.errors},
	}
}

// explain turns an error into a plain-language hint and a severity.
func explain(code string, status int, op string, replicated bool) (string, string) {
	switch code {
	case "NoSuchKey", "NotFound":
		if op == "HeadObject" {
			return "A client asked whether an object exists and it does not. Backup and sync tools do this before every upload; it is normal.", "info"
		}
		return "A client asked for an object that does not exist (deleted, never uploaded, or a wrong key).", "info"
	case "NoSuchBucket":
		if replicated && op == "HeadBucket" {
			return "Another server checked whether this bucket exists before creating it here. Replication does this once for every new bucket; it is normal.", "info"
		}
		return "The bucket named in the request does not exist on this server. Check the bucket name and the endpoint the client uses.", "warning"
	case "NoSuchVersion":
		return "The requested version id does not exist; it may have been permanently deleted.", "info"
	case "NoSuchUpload":
		return "A multipart upload id is unknown: the upload was completed, aborted or cleaned up before the client finished.", "warning"
	case "AccessDenied":
		return "The caller is authenticated but not allowed to do this: the user's role or bucket scope, a bucket policy, or an anonymous request to a private bucket.", "warning"
	case "InvalidAccessKeyId":
		return "The access key is unknown or disabled. A client is using an old or mistyped key.", "warning"
	case "SignatureDoesNotMatch":
		return "The request signature is wrong: usually a wrong secret key, or a proxy that rewrites the path, host or headers after the client signed them.", "warning"
	case "RequestTimeTooSkewed":
		return "The client's clock differs from the server's by more than 15 minutes. Fix the clock (NTP) on the client or the server.", "warning"
	case "AuthorizationHeaderMalformed", "MissingSecurityHeader", "InvalidRequest", "InvalidArgument", "MalformedXML", "InvalidPart", "InvalidPartOrder", "InvalidDigest", "BadDigest", "XAmzContentSHA256Mismatch":
		return "The request itself is invalid for S3: a client bug, an unsupported option, or data corrupted in transit. The message says which part.", "warning"
	case "IncompleteBody", "RequestTimeout":
		if replicated {
			return "An upload from another cellar member stopped midway. It is retried automatically; frequent occurrences point at an unstable link between members.", "info"
		}
		return "The client stopped sending before the upload was complete (it disconnected, timed out or was interrupted).", "warning"
	case "PreconditionFailed":
		return "A conditional request (If-Match / If-None-Match) did not hold. Clients use this on purpose to avoid overwriting; it is normal.", "info"
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return "A client tried to create a bucket that already exists. Many tools do this on start-up; it is harmless.", "info"
	case "BucketNotEmpty":
		return "A bucket could not be deleted because it still holds objects, old versions or delete markers. Use Delete everything in the bucket's settings.", "info"
	case "InvalidBucketName":
		return "The bucket name is not valid for S3 (3 to 63 lower-case letters, digits, dots and hyphens).", "info"
	case "InvalidBucketState":
		return "The operation does not fit the bucket's configuration, for example suspending versioning on an Object Lock bucket.", "info"
	case "ObjectLocked", "InvalidObjectState":
		return "A delete or overwrite was refused because the object is under retention or legal hold. That is Object Lock doing its job.", "info"
	case "EntityTooLarge", "EntityTooSmall":
		return "An upload is outside the allowed size (limits.max_object_size, or multipart parts below 5 MiB).", "warning"
	case "MethodNotAllowed":
		return "The HTTP method is not valid for that resource, for example a GET on a delete marker.", "info"
	case "NotImplemented":
		return "The client used an S3 feature this server does not implement.", "warning"
	case "SlowDown", "TooManyRequests":
		return "Too many failed authentication attempts from one address; the server is throttling it. Look for a client with wrong credentials.", "warning"
	case "ServiceUnavailable":
		return "A write was refused because not enough cellar members confirmed it (write acknowledgement). A member is down or unreachable; check the Cellar page.", "error"
	case "ReadOnly":
		return "The server refused writes because it is read-only.", "error"
	case "InternalError":
		return "The server failed while handling the request. Check the server log (journalctl -u hogshead) for the same request id.", "error"
	}
	if status >= 500 {
		return "The server failed while handling the request. Check the server log for the same request id.", "error"
	}
	return "The request was rejected. The message gives the reason.", "warning"
}

func (h *Handler) errors(c *Call) error {
	q := c.R.URL.Query()
	now := h.d.Now()
	from := now.Add(-24 * time.Hour)
	if v := q.Get("from"); v != "" {
		t, err := parseTime(v, now)
		if err != nil {
			return Errorf(http.StatusBadRequest, "InvalidArgument", "from: %v", err)
		}
		from = t
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	raw, err := h.d.DB.RequestErrors(c.Ctx(), from, usage.KeepErrors)
	if err != nil {
		return err
	}
	name := ""
	if h.d.Name != nil {
		name = h.d.Name()
	}
	status, _ := strconv.Atoi(q.Get("status"))
	var entries []ErrorEntry
	for _, d := range raw {
		var r usage.Record
		if json.Unmarshal([]byte(d), &r) != nil {
			continue
		}
		if (q.Get("bucket") != "" && r.Bucket != q.Get("bucket")) || (q.Get("user") != "" && r.User != q.Get("user")) ||
			(q.Get("code") != "" && !strings.EqualFold(r.ErrorCode, q.Get("code"))) || (status != 0 && r.Status != status) {
			continue
		}
		entries = append(entries, ErrorEntry{Record: r, Member: name})
	}
	out := ErrorsResponse{From: from, Scope: "local"}
	if q.Get("scope") == "cellar" && h.d.Cellar != nil && h.d.Cellar.Active(c.Ctx()) {
		out.Scope = "cellar"
		id := ""
		if h.d.ServerID != nil {
			id = h.d.ServerID()
		}
		out.Members = []UsageMember{{ID: id, Name: name, Self: true, OK: true}}
		peers, err := h.d.Cellar.Peers(c.Ctx())
		if err == nil {
			params := url.Values{}
			for k, vs := range q {
				if k != "scope" {
					params[k] = vs
				}
			}
			params.Set("limit", "1000")
			type answer struct {
				p   cellar.Peer
				res ErrorsResponse
				err error
			}
			ch := make(chan answer, len(peers))
			for _, p := range peers {
				go func(p cellar.Peer) {
					ctx, cancel := context.WithTimeout(c.Ctx(), 8*time.Second)
					defer cancel()
					var res ErrorsResponse
					err := p.Client.AdminJSON(ctx, http.MethodGet, "/errors?"+params.Encode(), nil, &res)
					ch <- answer{p, res, err}
				}(p)
			}
			for range peers {
				a := <-ch
				m := UsageMember{ID: a.p.ID, Name: a.p.Name, OK: a.err == nil}
				if a.err != nil {
					m.Error = a.err.Error()
				} else {
					for _, r := range a.res.Rows {
						r.Member = a.p.Name
						entries = append(entries, r)
					}
				}
				out.Members = append(out.Members, m)
			}
			sort.SliceStable(out.Members, func(i, j int) bool { return out.Members[i].Name < out.Members[j].Name })
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.After(entries[j].Time) })
	out.Total = len(entries)
	groups := map[string]*ErrorGroup{}
	var order []string
	for _, e := range entries {
		code := e.ErrorCode
		if code == "" {
			code = "HTTP " + strconv.Itoa(e.Status)
		}
		k := code + "|" + strconv.Itoa(e.Status) + "|" + e.Op + "|" + e.Bucket + "|" + e.User
		g := groups[k]
		if g == nil {
			hint, sev := explain(e.ErrorCode, e.Status, e.Op, e.Replicated || e.User == cellar.ServiceUser)
			g = &ErrorGroup{Code: code, Status: e.Status, Op: e.Op, Bucket: e.Bucket, User: e.User, LastSeen: e.Time, Message: e.ErrorMessage, Hint: hint, Severity: sev}
			groups[k] = g
			order = append(order, k)
		}
		g.Count++
		if e.Member != "" {
			seen := false
			for _, m := range g.Members {
				if m == e.Member {
					seen = true
				}
			}
			if !seen {
				g.Members = append(g.Members, e.Member)
			}
		}
	}
	out.Groups = make([]ErrorGroup, 0, len(order))
	for _, k := range order {
		out.Groups = append(out.Groups, *groups[k])
	}
	rank := map[string]int{"error": 0, "warning": 1, "info": 2}
	sort.SliceStable(out.Groups, func(i, j int) bool {
		if rank[out.Groups[i].Severity] != rank[out.Groups[j].Severity] {
			return rank[out.Groups[i].Severity] < rank[out.Groups[j].Severity]
		}
		return out.Groups[i].Count > out.Groups[j].Count
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out.Rows = entries
	if out.Rows == nil {
		out.Rows = []ErrorEntry{}
	}
	return c.JSON(http.StatusOK, out)
}
