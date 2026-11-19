package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/cellar"
	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/discovery"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/replication"
	"github.com/enetforge/hogshead/internal/s3client"
	"github.com/enetforge/hogshead/internal/transfer"
)

// Deps are the collaborators of the admin API.
type Deps struct {
	DB             *meta.DB
	Propose        func(ctx context.Context, cmd meta.Command) (meta.Result, error)
	Cfg            func() config.Config
	ConfigPath     string
	Reload         func() error
	PendingRestart func() []string
	Replication    *replication.Worker
	Transfers      *transfer.Manager // optional: import/export jobs
	Cellar         *cellar.Manager   // optional: cellar membership
	// ServerID and Name identify this server; Peers lists servers heard on
	// the LAN (nil when discovery is off).
	ServerID    func() string
	Name        func() string
	Peers       func() []discovery.Peer
	WorkerStats func() WorkerStats
	Version     string
	Started     time.Time
	Log         *slog.Logger
	Now         func() time.Time
	// Trusted marks a handler instance that serves the local unix socket:
	// every request is treated as coming from an administrator.
	Trusted bool
	// Extra lets callers register additional routes.
	Extra []Route
	// Session authenticates web UI requests by cookie; nil disables it.
	Session SessionAuth
	// RotateSessions replaces the web UI session secret, signing every
	// session out; nil when the web UI login is not wired.
	RotateSessions func(ctx context.Context) error
}

// SessionAuth resolves a web UI session cookie to a user; (nil, nil) means
// the request carries no session.
type SessionAuth interface {
	UserFromRequest(ctx context.Context, r *http.Request) (*meta.User, error)
}

// Route is one API endpoint; the table doubles as the OpenAPI source.
type Route struct {
	Method  string
	Path    string // pattern with {name} segments, relative to Prefix
	Summary string
	Admin   bool // requires admin role
	Handler func(c *Call) error
}

// Call is one request.
type Call struct {
	W      http.ResponseWriter
	R      *http.Request
	Params map[string]string
	User   *meta.User // nil only for trusted socket callers without a user
	Via    string
}

// Ctx returns the request context.
func (c *Call) Ctx() context.Context { return c.R.Context() }

// Actor names the caller for audit records.
func (c *Call) Actor() string {
	if c.User != nil {
		return c.User.Name
	}
	return "local"
}

// Handler serves the admin API.
type Handler struct {
	d        Deps
	verifier *auth.Verifier
	routes   []Route
	replMu   sync.Mutex // serialises read-merge-write of replication rules
}

// New builds the handler.
func New(d Deps) *Handler {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	h := &Handler{d: d}
	h.verifier = &auth.Verifier{
		Lookup: func(ctx context.Context, ak string) (string, error) {
			c, err := d.DB.LookupAccessKey(ctx, ak)
			if err != nil {
				if errors.Is(err, meta.ErrNoSuchKeyID) {
					return "", auth.ErrUnknownKey
				}
				return "", err
			}
			return c.Key.Secret, nil
		},
		Now:  d.Now,
		Skew: d.Cfg().Limits.ClockSkewWindow.Std(),
	}
	if h.d.Replication == nil {
		// A dormant worker still computes rule status from metadata.
		h.d.Replication = replication.New(replication.Deps{DB: d.DB, Propose: d.Propose}, 0)
	}
	routes := append(h.coreRoutes(), h.replicationRoutes()...)
	routes = append(routes, h.transferRoutes()...)
	routes = append(routes, h.cellarRoutes()...)
	routes = append(routes, h.usageRoutes()...)
	routes = append(routes, h.errorRoutes()...)
	h.routes = append(routes, d.Extra...)
	return h
}

// Routes returns the route table.
func (h *Handler) Routes() []Route { return h.routes }

// APIError is a JSON API error.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// Errorf builds an APIError.
func Errorf(status int, code, format string, args ...any) *APIError {
	return &APIError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path := strings.TrimPrefix(r.URL.Path, Prefix)
	if path == r.URL.Path {
		h.writeErr(w, Errorf(http.StatusNotFound, "NotFound", "unknown path"))
		return
	}
	route, params, allowed := h.match(r.Method, path)
	if route == nil {
		if allowed {
			h.writeErr(w, Errorf(http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed"))
		} else {
			h.writeErr(w, Errorf(http.StatusNotFound, "NotFound", "unknown path"))
		}
		return
	}
	c := &Call{W: w, R: r, Params: params}
	if err := h.authenticate(c); err != nil {
		h.writeErr(w, err)
		return
	}
	if route.Admin && c.Via != "socket" && (c.User == nil || c.User.Role != meta.RoleAdmin) {
		h.writeErr(w, Errorf(http.StatusForbidden, "Forbidden", "administrator role required"))
		return
	}
	if err := route.Handler(c); err != nil {
		h.writeErr(w, err)
	}
}

func (h *Handler) match(method, path string) (*Route, map[string]string, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	pathMatched := false
	for i := range h.routes {
		rt := &h.routes[i]
		psegs := strings.Split(strings.Trim(rt.Path, "/"), "/")
		if len(psegs) != len(segs) {
			continue
		}
		params := map[string]string{}
		ok := true
		for j, ps := range psegs {
			if strings.HasPrefix(ps, "{") && strings.HasSuffix(ps, "}") {
				params[ps[1:len(ps)-1]] = segs[j]
				continue
			}
			if ps != segs[j] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		pathMatched = true
		if rt.Method == method {
			return rt, params, true
		}
	}
	return nil, nil, pathMatched
}

func (h *Handler) authenticate(c *Call) error {
	if h.d.Trusted {
		c.Via = "socket"
		// Optionally identify as a user via header for audit purposes.
		if u := c.R.Header.Get("X-Hogshead-User"); u != "" {
			if user, err := h.d.DB.GetUserByName(c.Ctx(), u); err == nil {
				c.User = user
			}
		}
		return nil
	}
	ident, err := h.verifier.Verify(c.Ctx(), c.R)
	if err != nil {
		if errors.Is(err, auth.ErrMissingAuth) {
			return h.authenticateSession(c)
		}
		return Errorf(http.StatusForbidden, "Forbidden", "%v", err)
	}
	cred, err := h.d.DB.LookupAccessKey(c.Ctx(), ident.AccessKeyID)
	if err != nil {
		return Errorf(http.StatusForbidden, "Forbidden", "unknown access key")
	}
	if cred.Key.Disabled || cred.User.Disabled {
		return Errorf(http.StatusForbidden, "Forbidden", "access key disabled")
	}
	c.User = &cred.User
	c.Via = "sigv4"
	return nil
}

// authenticateSession accepts a web UI session cookie in place of SigV4.
func (h *Handler) authenticateSession(c *Call) error {
	if h.d.Session == nil {
		return Errorf(http.StatusUnauthorized, "Unauthorized", "authentication required")
	}
	u, err := h.d.Session.UserFromRequest(c.Ctx(), c.R)
	if err != nil {
		return Errorf(http.StatusForbidden, "Forbidden", "%v", err)
	}
	if u == nil {
		return Errorf(http.StatusUnauthorized, "Unauthorized", "authentication required")
	}
	c.User = u
	c.Via = "session"
	return nil
}

func (h *Handler) writeErr(w http.ResponseWriter, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		var me *meta.Error
		if errors.As(err, &me) {
			ae = &APIError{Status: statusForMeta(me.Code), Code: string(me.Code), Message: me.Msg}
		} else {
			h.d.Log.Error("admin request failed", "err", err)
			ae = Errorf(http.StatusInternalServerError, "InternalError", "internal error")
		}
	}
	var body ErrorBody
	body.Error.Code, body.Error.Message = ae.Code, ae.Message
	writeJSON(w, ae.Status, body)
}

func statusForMeta(code meta.Code) int {
	switch code {
	case meta.CodeNoSuchBucket, meta.CodeNoSuchKey, meta.CodeNoSuchUser, meta.CodeNoSuchKeyID, meta.CodeNoSuchUpload, meta.CodeNoSuchVersion, meta.CodeNoSuchTransfer:
		return http.StatusNotFound
	case meta.CodeBucketAlreadyExists, meta.CodeBucketAlreadyOwned, meta.CodeBucketNotEmpty, meta.CodeUserExists, meta.CodeInvalidBucketState:
		return http.StatusConflict
	case meta.CodeInvalidArgument, meta.CodePreconditionFailed:
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// JSON writes a success response.
func (c *Call) JSON(status int, v any) error {
	writeJSON(c.W, status, v)
	return nil
}

// Decode parses the JSON body into v.
func (c *Call) Decode(v any) error {
	b, err := io.ReadAll(io.LimitReader(c.R.Body, 1<<20))
	if err != nil {
		return Errorf(http.StatusBadRequest, "BadRequest", "read body: %v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return Errorf(http.StatusBadRequest, "BadRequest", "request body required")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Errorf(http.StatusBadRequest, "BadRequest", "invalid JSON: %v", err)
	}
	return nil
}

func (h *Handler) propose(c *Call, cmd meta.Command) (meta.Result, error) {
	if o := c.R.Header.Get(s3client.OriginHeader); o != "" {
		wrapped, err := meta.Wrap(o, cmd)
		if err != nil {
			return meta.Result{}, err
		}
		cmd = wrapped
	}
	res, err := h.d.Propose(c.Ctx(), cmd)
	if err != nil {
		return res, err
	}
	if res.Err != "" {
		return res, res.Error()
	}
	return res, nil
}

func (h *Handler) audit(c *Call, action, target, detail string) {
	_, _ = h.d.Propose(c.Ctx(), &meta.Audit{Actor: c.Actor(), Action: action, Target: target, Detail: detail, Now: h.d.Now().UnixMilli()})
}

// ---- conversions ----

func userOut(u *meta.User) User {
	b := u.Buckets
	if b == nil {
		b = []string{}
	}
	return User{ID: u.ID, Name: u.Name, Role: u.Role, Buckets: b, Disabled: u.Disabled, CreatedAt: u.CreatedAt, HasPassword: u.PasswordHash != ""}
}

func keyOut(k meta.AccessKey, userName string) AccessKey {
	return AccessKey{AccessKeyID: k.AccessKeyID, User: userName, Label: k.Label, Disabled: k.Disabled, CreatedAt: k.CreatedAt}
}

func (h *Handler) bucketOut(ctx context.Context, b *meta.Bucket, full bool) Bucket {
	out := Bucket{
		Name: b.Name, Owner: b.Owner, CreatedAt: b.CreatedAt, Versioning: b.Versioning, ObjectLock: b.ObjectLock,
	}
	if u, err := h.d.DB.GetUser(ctx, b.Owner); err == nil {
		out.OwnerName = u.Name
	}
	if full {
		out.Policy, out.CORS, out.Lifecycle = b.Policy, b.CORS, b.Lifecycle
	}
	if st, err := h.d.DB.BucketStats(ctx, b.Name); err == nil {
		out.Objects, out.Bytes = st.Objects, st.Bytes
	}
	if v, m, err := h.d.DB.BucketVersionStats(ctx, b.Name); err == nil {
		out.Versions, out.DeleteMarkers = v, m
	}
	return out
}
