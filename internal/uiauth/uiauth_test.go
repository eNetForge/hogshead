package uiauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/api"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/session"
	"github.com/enetforge/hogshead/internal/testutil"
)

// env is a server with the S3 API, the admin API and the login endpoints
// sharing one session manager, as wire builds it.
type env struct {
	n   *testutil.Node
	srv *httptest.Server
	jar []*http.Cookie
}

func start(t *testing.T) *env {
	n := testutil.Start(t)
	ctx := context.Background()
	mgr, err := session.New([]byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	authn := &session.Authenticator{Manager: mgr, Lookup: n.Core.DB.GetUser}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s3 := api.New(api.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Cfg: n.Core.Cfg, Log: log, Region: "us-east-1"})
	s3.SetSession(authn)
	adm := admin.New(admin.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Cfg: n.Core.Cfg, Log: log, Session: authn, Started: time.Now()})
	ui := New(Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Sessions: mgr, Log: log})
	mux := http.NewServeMux()
	mux.Handle(admin.Prefix+"/", adm)
	mux.Handle(Prefix+"/", ui)
	mux.Handle("/", s3)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// alice: read-write with a password; no access key at all.
	uid := "u-alice"
	if _, err := n.Core.Propose(ctx, &meta.CreateUser{ID: uid, Name: "alice", Role: meta.RoleReadWrite, Now: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	hash, err := admin.HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Core.Propose(ctx, &meta.SetUserPassword{ID: uid, Hash: hash}); err != nil {
		t.Fatal(err)
	}
	return &env{n: n, srv: srv}
}

func (e *env) do(t *testing.T, method, path string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	for _, c := range e.jar {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if cs := resp.Cookies(); len(cs) > 0 {
		e.jar = cs
	}
	return resp, b
}

func TestPasswordLoginFlow(t *testing.T) {
	e := start(t)
	js := map[string]string{"content-type": "application/json"}
	withHdr := map[string]string{session.Header: "1"}

	// Wrong password, unknown user, missing password login, then success.
	if r, b := e.do(t, "POST", Prefix+"/login", []byte(`{"user":"alice","password":"nope"}`), js); r.StatusCode != 401 {
		t.Fatalf("wrong password: %d %s", r.StatusCode, b)
	}
	if r, _ := e.do(t, "POST", Prefix+"/login", []byte(`{"user":"nobody","password":"correct horse"}`), js); r.StatusCode != 401 {
		t.Fatalf("unknown user: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", Prefix+"/login", []byte(`{"user":"admin","password":"correct horse"}`), js); r.StatusCode != 401 {
		t.Fatalf("user without password: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", Prefix+"/me", nil, nil); r.StatusCode != 401 {
		t.Fatalf("me before login: %d", r.StatusCode)
	}
	r, b := e.do(t, "POST", Prefix+"/login", []byte(`{"user":"alice","password":"correct horse"}`), js)
	if r.StatusCode != 200 || len(e.jar) == 0 || e.jar[0].Name != session.CookieName || !e.jar[0].HttpOnly {
		t.Fatalf("login: %d %s cookies=%v", r.StatusCode, b, e.jar)
	}
	var me admin.Me
	_ = json.Unmarshal(b, &me)
	if me.User.Name != "alice" || me.Via != "session" || !me.User.HasPassword {
		t.Fatalf("me: %+v", me)
	}

	// The cookie authenticates the admin API and the S3 API.
	if r, b := e.do(t, "GET", admin.Prefix+"/me", nil, nil); r.StatusCode != 200 || !strings.Contains(string(b), `"via":"session"`) {
		t.Fatalf("admin me via cookie: %d %s", r.StatusCode, b)
	}
	if r, b := e.do(t, "PUT", "/photos", nil, nil); r.StatusCode != 403 || !strings.Contains(string(b), session.Header) {
		t.Fatalf("PUT without header must be refused: %d %s", r.StatusCode, b)
	}
	if r, b := e.do(t, "PUT", "/photos", nil, withHdr); r.StatusCode != 200 {
		t.Fatalf("create bucket via cookie: %d %s", r.StatusCode, b)
	}
	if r, b := e.do(t, "PUT", "/photos/a.txt", []byte("hi"), map[string]string{session.Header: "1", "Content-Type": "text/plain"}); r.StatusCode != 200 {
		t.Fatalf("put object via cookie: %d %s", r.StatusCode, b)
	}
	if r, b := e.do(t, "GET", "/photos/a.txt", nil, nil); r.StatusCode != 200 || string(b) != "hi" {
		t.Fatalf("get via cookie: %d %s", r.StatusCode, b)
	}
	if r, _ := e.do(t, "GET", "/photos/a.txt", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.StatusCode != 403 {
		t.Fatalf("cross-site GET must be refused: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "PUT", "/photos/b.txt", []byte("x"), map[string]string{session.Header: "1", "Origin": "http://evil.example"}); r.StatusCode != 403 {
		t.Fatalf("foreign origin must be refused: %d", r.StatusCode)
	}
	// Owner of the bucket is the session user.
	if r, b := e.do(t, "GET", "/", nil, nil); r.StatusCode != 200 || !strings.Contains(string(b), "alice") || !strings.Contains(string(b), "photos") {
		t.Fatalf("list buckets: %d %s", r.StatusCode, b)
	}

	// Presign creates a key for alice on demand and the URL works anonymously.
	r, b = e.do(t, "POST", Prefix+"/presign", []byte(`{"bucket":"photos","key":"a.txt","expires":60}`), map[string]string{session.Header: "1", "content-type": "application/json"})
	if r.StatusCode != 200 {
		t.Fatalf("presign: %d %s", r.StatusCode, b)
	}
	var pre struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(b, &pre)
	resp, err := http.Get(pre.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hi" {
		t.Fatalf("presigned GET: %d %s (%s)", resp.StatusCode, body, pre.URL)
	}
	keys, _ := e.n.Core.DB.ListAccessKeys(context.Background(), "u-alice")
	if len(keys) != 1 || keys[0].Label != "ui" {
		t.Fatalf("ui key: %+v", keys)
	}

	// Self password change via the admin API with the cookie; then the old one fails.
	if r, b := e.do(t, "PUT", admin.Prefix+"/users/alice/password", []byte(`{"password":"short"}`), map[string]string{session.Header: "1", "content-type": "application/json"}); r.StatusCode != 400 {
		t.Fatalf("short password: %d %s", r.StatusCode, b)
	}
	if r, b := e.do(t, "PUT", admin.Prefix+"/users/admin/password", []byte(`{"password":"long enough"}`), map[string]string{session.Header: "1", "content-type": "application/json"}); r.StatusCode != 403 {
		t.Fatalf("non-admin setting another password: %d %s", r.StatusCode, b)
	}
	if r, b := e.do(t, "PUT", admin.Prefix+"/users/alice/password", []byte(`{"password":"new password"}`), map[string]string{session.Header: "1", "content-type": "application/json"}); r.StatusCode != 200 {
		t.Fatalf("self password: %d %s", r.StatusCode, b)
	}
	// Logout clears the cookie.
	r, _ = e.do(t, "POST", Prefix+"/logout", nil, nil)
	if r.StatusCode != 204 || e.jar[0].Value != "" {
		t.Fatalf("logout: %d %v", r.StatusCode, e.jar)
	}
	e.jar = nil
	if r, _ := e.do(t, "POST", Prefix+"/login", []byte(`{"user":"alice","password":"correct horse"}`), js); r.StatusCode != 401 {
		t.Fatalf("old password still works: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", Prefix+"/login", []byte(`{"user":"alice","password":"new password"}`), js); r.StatusCode != 200 {
		t.Fatalf("new password: %d", r.StatusCode)
	}

	// A disabled user's cookie stops working.
	if _, err := e.n.Core.Propose(context.Background(), &meta.UpdateUser{ID: "u-alice", Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.do(t, "GET", admin.Prefix+"/me", nil, nil); r.StatusCode != 403 {
		t.Fatalf("disabled user via cookie: %d", r.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := start(t)
	js := map[string]string{"content-type": "application/json"}
	for i := 0; i < 10; i++ {
		e.do(t, "POST", Prefix+"/login", []byte(`{"user":"alice","password":"wrong"}`), js)
	}
	if r, _ := e.do(t, "POST", Prefix+"/login", []byte(`{"user":"alice","password":"correct horse"}`), js); r.StatusCode != 429 {
		t.Fatalf("expected rate limit, got %d", r.StatusCode)
	}
}

func ptr[T any](v T) *T { return &v }
