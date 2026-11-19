package admin

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/enetforge/hogshead/internal/cellar"
	"github.com/enetforge/hogshead/internal/meta"
)

// UsageResponse is the answer to GET /usage.
type UsageResponse struct {
	From   time.Time       `json:"from"`
	To     time.Time       `json:"to"`
	By     []string        `json:"by"`
	Rows   []meta.UsageRow `json:"rows"`
	Totals meta.UsageRow   `json:"totals"`
	// AccessLog is the configured access log path, "" when off.
	AccessLog string `json:"access_log,omitempty"`
	// Scope is "local" or "cellar"; Members lists each member's part of a
	// cellar-wide answer, including the ones that could not be reached.
	Scope   string        `json:"scope"`
	Members []UsageMember `json:"members,omitempty"`
}

// UsageMember is one member's contribution to a cellar-wide query.
type UsageMember struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Self  bool   `json:"self"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (h *Handler) usageRoutes() []Route {
	return []Route{
		{"GET", "/usage", "S3 usage counters: requests, errors and bytes by bucket, user, access key, class, hour or member (?from=&to=&by=&bucket=&user=&key=&scope=local|cellar)", true, h.usage},
	}
}

func (h *Handler) usage(c *Call) error {
	q := c.R.URL.Query()
	now := h.d.Now()
	f := meta.UsageFilter{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour), Bucket: q.Get("bucket"), User: q.Get("user"), AccessKey: q.Get("key")}
	if v := q.Get("from"); v != "" {
		t, err := parseTime(v, now)
		if err != nil {
			return Errorf(http.StatusBadRequest, "InvalidArgument", "from: %v", err)
		}
		f.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := parseTime(v, now)
		if err != nil {
			return Errorf(http.StatusBadRequest, "InvalidArgument", "to: %v", err)
		}
		f.To = t
	}
	var by []string
	if v := q.Get("by"); v != "" {
		for _, d := range strings.Split(v, ",") {
			if d = strings.TrimSpace(d); d != "" {
				by = append(by, d)
			}
		}
	}
	// "member" is a virtual dimension: rows are tagged per member instead of merged.
	byMember := false
	var localBy []string
	for _, d := range by {
		if d == "member" {
			byMember = true
			continue
		}
		localBy = append(localBy, d)
	}
	rows, err := h.d.DB.Usage(c.Ctx(), f, localBy)
	if err != nil {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "%v", err)
	}
	totals, err := h.d.DB.Usage(c.Ctx(), f, nil)
	if err != nil {
		return err
	}
	out := UsageResponse{From: f.From, To: f.To, By: by, Rows: rows, AccessLog: h.d.Cfg().AccessLog, Scope: "local"}
	if out.By == nil {
		out.By = []string{}
	}
	if out.Rows == nil {
		out.Rows = []meta.UsageRow{}
	}
	if len(totals) == 1 {
		out.Totals = totals[0]
	}
	if q.Get("scope") == "cellar" && h.d.Cellar != nil && h.d.Cellar.Active(c.Ctx()) {
		return c.JSON(http.StatusOK, h.cellarUsage(c, out, q, byMember))
	}
	if byMember {
		name := ""
		if h.d.Name != nil {
			name = h.d.Name()
		}
		for i := range out.Rows {
			out.Rows[i].Member = name
		}
	}
	return c.JSON(http.StatusOK, out)
}

// cellarUsage asks every other member for the same window and merges the
// answers with the local one: summed per group key, or tagged per member
// when "member" is among the dimensions. Unreachable members are reported
// and left out of the sums.
func (h *Handler) cellarUsage(c *Call, local UsageResponse, q url.Values, byMember bool) UsageResponse {
	out := local
	out.Scope = "cellar"
	selfName := ""
	if h.d.Name != nil {
		selfName = h.d.Name()
	}
	selfID := ""
	if h.d.ServerID != nil {
		selfID = h.d.ServerID()
	}
	out.Members = []UsageMember{{ID: selfID, Name: selfName, Self: true, OK: true}}
	peers, err := h.d.Cellar.Peers(c.Ctx())
	if err != nil {
		out.Members[0].Error = err.Error()
		return out
	}
	params := url.Values{}
	for k, vs := range q {
		if k != "scope" {
			params[k] = vs
		}
	}
	type answer struct {
		peer cellar.Peer
		res  UsageResponse
		err  error
	}
	answers := make(chan answer, len(peers))
	for _, p := range peers {
		go func(p cellar.Peer) {
			ctx, cancel := context.WithTimeout(c.Ctx(), 8*time.Second)
			defer cancel()
			var res UsageResponse
			err := p.Client.AdminJSON(ctx, http.MethodGet, "/usage?"+params.Encode(), nil, &res)
			answers <- answer{peer: p, res: res, err: err}
		}(p)
	}
	// Merge rows by their group key (member included when requested).
	merged := map[string]*meta.UsageRow{}
	var order []string
	add := func(r meta.UsageRow, member string) {
		if byMember {
			r.Member = member
		}
		k := strconv.FormatInt(r.Hour, 10) + "|" + r.Bucket + "|" + r.User + "|" + r.AccessKey + "|" + r.Class + "|" + r.Member
		if cur := merged[k]; cur != nil {
			cur.Requests += r.Requests
			cur.Errors += r.Errors
			cur.BytesIn += r.BytesIn
			cur.BytesOut += r.BytesOut
			return
		}
		cp := r
		merged[k] = &cp
		order = append(order, k)
	}
	for _, r := range local.Rows {
		add(r, selfName)
	}
	totals := local.Totals
	for range peers {
		a := <-answers
		m := UsageMember{ID: a.peer.ID, Name: a.peer.Name, OK: a.err == nil}
		if a.err != nil {
			m.Error = a.err.Error()
		} else {
			for _, r := range a.res.Rows {
				add(r, a.peer.Name)
			}
			totals.Requests += a.res.Totals.Requests
			totals.Errors += a.res.Totals.Errors
			totals.BytesIn += a.res.Totals.BytesIn
			totals.BytesOut += a.res.Totals.BytesOut
		}
		out.Members = append(out.Members, m)
	}
	rows := make([]meta.UsageRow, 0, len(order))
	for _, k := range order {
		rows = append(rows, *merged[k])
	}
	byHour := false
	for _, d := range local.By {
		if d == "hour" {
			byHour = true
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if byHour && rows[i].Hour != rows[j].Hour {
			return rows[i].Hour < rows[j].Hour
		}
		return rows[i].Requests > rows[j].Requests
	})
	out.Rows = rows
	out.Totals = totals
	sort.SliceStable(out.Members, func(i, j int) bool { return out.Members[i].Name < out.Members[j].Name })
	return out
}

// parseTime accepts RFC 3339, a unix timestamp, or a relative duration such
// as "24h" or "7d" meaning that long before now.
func parseTime(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if strings.HasSuffix(v, "d") {
		if d, err := time.ParseDuration(strings.TrimSuffix(v, "d") + "h"); err == nil {
			return now.Add(-d * 24), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d), nil
	}
	var unix int64
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return time.Time{}, errUsageTime
		}
		unix = unix*10 + int64(ch-'0')
	}
	if unix == 0 {
		return time.Time{}, errUsageTime
	}
	return time.Unix(unix, 0), nil
}

type usageTimeError struct{}

func (usageTimeError) Error() string {
	return "use RFC 3339, a unix timestamp, or a duration before now such as 24h or 7d"
}

var errUsageTime error = usageTimeError{}
