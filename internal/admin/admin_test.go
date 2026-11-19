package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/adminclient"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/testutil"
)

func setup(t *testing.T, trusted bool) (*testutil.Node, *adminclient.Client, *httptest.Server) {
	t.Helper()
	n := testutil.Start(t)
	reloads := 0
	h := admin.New(admin.Deps{
		DB: n.Core.DB, Propose: n.Core.ProposeRaw, Cfg: n.Core.Cfg, ConfigPath: "/x/config.toml",
		Reload:         func() error { reloads++; return nil },
		PendingRestart: func() []string { return []string{"listen"} },
		Version:        "test", Started: time.Now(), Trusted: trusted,
		WorkerStats: func() admin.WorkerStats { return admin.WorkerStats{ChunksReleased: 3} },
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var c *adminclient.Client
	if trusted {
		// Point a socket-style client at the HTTP server by reusing the transport.
		c = clientWithoutCreds(srv.URL)
	} else {
		c = adminclient.NewHTTP(srv.URL, n.Admin)
	}
	return n, c, srv
}

func TestAdminFlow(t *testing.T) {
	n, c, srv := setup(t, false)
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.Workers.ChunksReleased != 3 || st.PendingRestart[0] != "listen" {
		t.Fatalf("status: %+v", st)
	}
	me, _ := c.Me(ctx)
	if me.User.Name != "admin" || me.Via != "sigv4" {
		t.Fatalf("me: %+v", me)
	}
	// Users.
	u, err := c.CreateUser(ctx, admin.CreateUserRequest{Name: "bob", Role: meta.RoleReadOnly, Buckets: []string{"photos"}})
	if err != nil || u.Role != meta.RoleReadOnly || u.Buckets[0] != "photos" {
		t.Fatalf("create user: %+v %v", u, err)
	}
	if _, err := c.CreateUser(ctx, admin.CreateUserRequest{Name: "bob"}); err == nil {
		t.Fatal("duplicate user")
	}
	if _, err := c.CreateUser(ctx, admin.CreateUserRequest{Name: "bad name!"}); err == nil {
		t.Fatal("invalid user name")
	}
	if _, err := c.CreateUser(ctx, admin.CreateUserRequest{Name: "x", Role: "god"}); err == nil {
		t.Fatal("invalid role")
	}
	users, _ := c.ListUsers(ctx)
	if len(users) != 2 {
		t.Fatalf("users: %+v", users)
	}
	role := meta.RoleReadWrite
	all := []string{}
	u, err = c.UpdateUser(ctx, "bob", admin.UpdateUserRequest{Role: &role, Buckets: &all})
	if err != nil || u.Role != meta.RoleReadWrite || len(u.Buckets) != 0 {
		t.Fatalf("update user: %+v %v", u, err)
	}
	dis := true
	if _, err := c.UpdateUser(ctx, "admin", admin.UpdateUserRequest{Disabled: &dis}); err == nil {
		t.Fatal("cannot disable self")
	}
	if err := c.DeleteUser(ctx, "admin"); err == nil {
		t.Fatal("cannot delete self")
	}
	if _, err := c.GetUser(ctx, "nobody"); err == nil {
		t.Fatal("missing user")
	}
	// Keys.
	k, err := c.CreateKey(ctx, "bob", "laptop")
	if err != nil || k.SecretAccessKey == "" || k.Label != "laptop" {
		t.Fatalf("create key: %+v %v", k, err)
	}
	keys, _ := c.ListKeys(ctx, "bob")
	if len(keys) != 1 || keys[0].AccessKeyID != k.AccessKeyID {
		t.Fatalf("keys: %+v", keys)
	}
	allKeys, _ := c.ListKeys(ctx, "")
	if len(allKeys) != 2 {
		t.Fatalf("all keys: %+v", allKeys)
	}
	kk, err := c.UpdateKey(ctx, k.AccessKeyID, true)
	if err != nil || !kk.Disabled {
		t.Fatal("disable key")
	}
	bu, _ := c.GetUser(ctx, "bob")
	if bu.Keys != 1 {
		t.Fatal("key count")
	}
	// Buckets.
	b, err := c.CreateBucket(ctx, admin.CreateBucketRequest{Name: "photos", Versioning: "Enabled", Owner: "bob"})
	if err != nil || b.OwnerName != "bob" || b.Versioning != "Enabled" {
		t.Fatalf("create bucket: %+v %v", b, err)
	}
	if _, err := c.CreateBucket(ctx, admin.CreateBucketRequest{Name: "Bad"}); err == nil {
		t.Fatal("invalid bucket name")
	}
	if _, err := c.CreateBucket(ctx, admin.CreateBucketRequest{Name: "photos"}); err == nil {
		t.Fatal("duplicate bucket")
	}
	pub := true
	cors := `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`
	lc := `<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	b, err = c.UpdateBucket(ctx, "photos", admin.UpdateBucketRequest{PublicRead: &pub, CORS: &cors, Lifecycle: &lc})
	if err != nil || b.Policy == "" || !strings.Contains(b.CORS, "AllowedOrigin") || !strings.Contains(b.Lifecycle, "Days") {
		t.Fatalf("update bucket: %+v %v", b, err)
	}
	badPol := `{"Statement":[]}`
	if _, err := c.UpdateBucket(ctx, "photos", admin.UpdateBucketRequest{Policy: &badPol}); err == nil {
		t.Fatal("bad policy must fail")
	}
	badCors := `<CORSConfiguration></CORSConfiguration>`
	if _, err := c.UpdateBucket(ctx, "photos", admin.UpdateBucketRequest{CORS: &badCors}); err == nil {
		t.Fatal("bad cors must fail")
	}
	empty := ""
	b, _ = c.UpdateBucket(ctx, "photos", admin.UpdateBucketRequest{Policy: &empty})
	if b.Policy != "" {
		t.Fatalf("clear: %+v", b)
	}
	bs, _ := c.ListBuckets(ctx)
	if len(bs) != 1 || bs[0].Policy != "" {
		t.Fatalf("list buckets: %+v", bs)
	}
	if _, err := c.GetBucket(ctx, "nope"); err == nil {
		t.Fatal("missing bucket")
	}
	if err := c.DeleteBucket(ctx, "photos", false); err != nil {
		t.Fatal(err)
	}
	// Settings.
	if err := c.SetSetting(ctx, admin.SettingGCGrace, "48h"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSetting(ctx, admin.SettingGCGrace, "soon"); err == nil {
		t.Fatal("invalid duration")
	}
	ss, _ := c.Settings(ctx)
	found := false
	for _, s := range ss {
		if s.Key == admin.SettingGCGrace && s.Value == "48h" {
			found = true
		}
	}
	if !found {
		t.Fatalf("settings: %+v", ss)
	}
	// Audit.
	es, _ := c.Audit(ctx, 100, 0)
	if len(es) < 8 || es[0].Action != "setting.set" {
		t.Fatalf("audit: %d %+v", len(es), es)
	}
	// Config.
	cv, err := c.Config(ctx)
	if err != nil || !strings.Contains(cv.TOML, "data_dir") || cv.PendingRestart[0] != "listen" {
		t.Fatalf("config: %+v %v", cv, err)
	}
	if _, err := c.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	doc, err := c.OpenAPI(ctx)
	if err != nil || doc["openapi"] != "3.0.3" {
		t.Fatalf("openapi: %v %v", doc, err)
	}
	// Non-admin is forbidden from admin routes but may call /me.
	rw := n.CreateUser("rw", meta.RoleReadWrite, nil)
	rc := adminclient.NewHTTP(srv.URL, rw)
	if _, err := rc.ListUsers(ctx); err == nil {
		t.Fatal("non-admin must be forbidden")
	}
	if m, err := rc.Me(ctx); err != nil || m.User.Name != "rw" {
		t.Fatalf("me as rw: %+v %v", m, err)
	}
	// Unauthenticated and wrong paths.
	resp, _ := http.Get(srv.URL + admin.Prefix + "/users")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/other")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatal("outside prefix")
	}
	req, _ := http.NewRequest("PUT", srv.URL+admin.Prefix+"/users", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", resp.StatusCode)
	}
}

func TestTrustedSocketHandler(t *testing.T) {
	_, c, _ := setup(t, true)
	ctx := context.Background()
	me, err := c.Me(ctx)
	if err != nil || me.Via != "socket" || me.User.Role != meta.RoleAdmin {
		t.Fatalf("socket me: %+v %v", me, err)
	}
	if _, err := c.CreateUser(ctx, admin.CreateUserRequest{Name: "viasocket"}); err != nil {
		t.Fatal(err)
	}
	cu := c.WithUser("admin")
	me, _ = cu.Me(ctx)
	if me.User.Name != "admin" {
		t.Fatalf("identity hint: %+v", me)
	}
	var out map[string]any
	if err := c.Raw(ctx, "GET", "/status", nil, &out); err != nil || out["version"] != "test" {
		t.Fatal("raw")
	}
	// Bad JSON body.
	err = c.Raw(ctx, "POST", "/users", json.RawMessage(`{"nope":1}`), nil)
	if err == nil || !strings.Contains(err.Error(), "BadRequest") {
		t.Fatalf("unknown field: %v", err)
	}
}
