// Package uiauth serves the small JSON API behind the web UI's password
// login: /_ui/api/login, logout, me and presign. Everything else the UI does
// goes through the S3 and admin APIs with the session cookie.
package uiauth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/session"
)

// Prefix is where the handler is mounted.
const Prefix = "/_ui/api"

// Deps are the handler's collaborators.
type Deps struct {
	DB       *meta.DB
	Propose  func(ctx context.Context, cmd meta.Command) (meta.Result, error)
	Sessions *session.Manager
	// Secure marks cookies Secure; set it when the server speaks TLS.
	Secure bool
	Region string
	Now    func() time.Time
	Log    *slog.Logger
}

// Handler serves the endpoints.
type Handler struct {
	d       Deps
	limiter *session.Limiter
	auth    *session.Authenticator
}

// New builds the handler.
func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Region == "" {
		d.Region = "us-east-1"
	}
	return &Handler{
		d:       d,
		limiter: session.NewLimiter(10, 15*time.Minute),
		auth:    &session.Authenticator{Manager: d.Sessions, Lookup: d.DB.GetUser},
	}
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	var e apiError
	e.Error.Code, e.Error.Message = code, msg
	writeJSON(w, status, e)
}

func userOut(u *meta.User) admin.User {
	b := u.Buckets
	if b == nil {
		b = []string{}
	}
	return admin.User{ID: u.ID, Name: u.Name, Role: u.Role, Buckets: b, Disabled: u.Disabled, CreatedAt: u.CreatedAt, HasPassword: u.PasswordHash != ""}
}

// ServeHTTP routes the endpoints under Prefix.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, Prefix) {
	case "/login":
		h.login(w, r)
	case "/logout":
		h.logout(w, r)
	case "/me":
		h.me(w, r)
	case "/presign":
		h.presign(w, r)
	default:
		writeErr(w, http.StatusNotFound, "NotFound", "no such endpoint")
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "POST only")
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		writeErr(w, http.StatusForbidden, "Forbidden", "cross-site login refused")
		return
	}
	ip := clientIP(r)
	now := h.d.Now()
	if h.limiter.Blocked(ip, now) {
		writeErr(w, http.StatusTooManyRequests, "SlowDown", "too many failed logins; try again later")
		return
	}
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.User == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "InvalidArgument", "user and password are required")
		return
	}
	u, err := h.d.DB.GetUserByName(r.Context(), req.User)
	if err != nil || u.PasswordHash == "" || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		h.limiter.Fail(ip, now)
		writeErr(w, http.StatusUnauthorized, "Unauthorized", "invalid user name or password")
		return
	}
	if u.Disabled {
		h.limiter.Fail(ip, now)
		writeErr(w, http.StatusForbidden, "Forbidden", "this user is disabled")
		return
	}
	h.limiter.Reset(ip)
	token, exp := h.d.Sessions.Issue(u.ID)
	session.SetCookie(w, token, exp, h.d.Secure)
	_, _ = h.d.Propose(r.Context(), &meta.Audit{Actor: u.Name, Action: "session.login", Target: u.Name, Detail: ip, Now: now.UnixMilli()})
	writeJSON(w, http.StatusOK, admin.Me{User: userOut(u), Via: "session"})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "POST only")
		return
	}
	session.ClearCookie(w, h.d.Secure)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sessionUser(w http.ResponseWriter, r *http.Request) *meta.User {
	u, err := h.auth.UserFromRequest(r.Context(), r)
	if err != nil {
		writeErr(w, http.StatusForbidden, "Forbidden", err.Error())
		return nil
	}
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "Unauthorized", "not logged in")
		return nil
	}
	return u
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u := h.sessionUser(w, r)
	if u == nil {
		return
	}
	writeJSON(w, http.StatusOK, admin.Me{User: userOut(u), Via: "session"})
}

// presign signs a GET URL for the session user with one of their access
// keys, creating a key labelled "ui" when they have none. The URL is checked
// like any other presigned request when it is used.
func (h *Handler) presign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "POST only")
		return
	}
	u := h.sessionUser(w, r)
	if u == nil {
		return
	}
	var req struct {
		Bucket    string `json:"bucket"`
		Key       string `json:"key"`
		VersionID string `json:"version_id"`
		Expires   int    `json:"expires"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Bucket == "" || req.Key == "" {
		writeErr(w, http.StatusBadRequest, "InvalidArgument", "bucket and key are required")
		return
	}
	if req.Expires <= 0 {
		req.Expires = 300
	}
	if req.Expires > 7*24*3600 {
		req.Expires = 7 * 24 * 3600
	}
	creds, err := h.keyFor(r.Context(), u)
	if err != nil {
		h.d.Log.Error("presign: no usable key", "user", u.Name, "err", err)
		writeErr(w, http.StatusInternalServerError, "InternalError", "could not obtain a signing key")
		return
	}
	scheme := "http"
	if r.TLS != nil || h.d.Secure || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	target := &url.URL{Scheme: scheme, Host: r.Host, Path: "/" + req.Bucket + "/" + req.Key, RawPath: "/" + url.PathEscape(req.Bucket) + "/" + escapeKey(req.Key)}
	if req.VersionID != "" {
		target.RawQuery = "versionId=" + url.QueryEscape(req.VersionID)
	}
	preq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidArgument", err.Error())
		return
	}
	signed, err := auth.Presign(preq, creds, h.d.Region, "s3", h.d.Now(), time.Duration(req.Expires)*time.Second)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": signed.String()})
}

func escapeKey(k string) string {
	parts := strings.Split(k, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (h *Handler) keyFor(ctx context.Context, u *meta.User) (auth.Credentials, error) {
	keys, err := h.d.DB.ListAccessKeys(ctx, u.ID)
	if err != nil {
		return auth.Credentials{}, err
	}
	for _, k := range keys {
		if !k.Disabled {
			return auth.Credentials{AccessKeyID: k.AccessKeyID, SecretKey: k.Secret}, nil
		}
	}
	c := auth.Credentials{AccessKeyID: ids.AccessKeyID(), SecretKey: ids.SecretKey()}
	if _, err := h.d.Propose(ctx, &meta.CreateAccessKey{AccessKeyID: c.AccessKeyID, Secret: c.SecretKey, UserID: u.ID, Label: "ui", Now: h.d.Now().UnixMilli()}); err != nil {
		return auth.Credentials{}, err
	}
	return c, nil
}
