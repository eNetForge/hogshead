package replication_test

import (
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
	"github.com/enetforge/hogshead/internal/replication"
	"github.com/enetforge/hogshead/internal/testutil"
)

// full serves a node's S3 API and admin API from one address, as wire does,
// so config sync can reach the admin API with the same credentials.
func full(t *testing.T, n *testutil.Node) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s3 := api.New(api.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Cfg: n.Core.Cfg, Log: log, Region: "us-east-1"})
	adm := admin.New(admin.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Cfg: n.Core.Cfg, Log: log, Started: time.Now()})
	mux := http.NewServeMux()
	mux.Handle(admin.Prefix+"/", adm)
	mux.Handle("/", s3)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestMesh runs two real servers replicating to each other with config
// sync: writes and users flow both ways exactly once.
func TestMesh(t *testing.T) {
	a := testutil.Start(t)
	b := testutil.Start(t)
	ctx := context.Background()
	aFull, bFull := full(t, a), full(t, b)
	urls := map[*testutil.Node]string{a: aFull.URL, b: bFull.URL}
	link := func(from, to *testutil.Node, id string) *replication.Worker {
		rule := replication.Rule{Targets: []replication.Target{{Name: "peer", Enabled: true, SyncConfig: true, Endpoint: urls[to], AccessKey: to.Admin.AccessKeyID, SecretKey: to.Admin.SecretKey, PathStyle: true}}}
		raw, _ := json.Marshal(rule)
		if _, err := from.Core.Propose(ctx, &meta.SetSetting{Key: replication.GlobalKey, Value: string(raw)}); err != nil {
			t.Fatal(err)
		}
		return replication.New(replication.Deps{DB: from.Core.DB, Propose: from.Core.ProposeRaw, Chunks: from.Core.Chunks, Log: slog.Default(), ServerID: func() string { return id }}, time.Hour)
	}
	wa := link(a, b, "s-a")
	wb := link(b, a, "s-b")
	settle := func() {
		for i := 0; i < 3; i++ {
			if err := wa.Pass(ctx); err != nil {
				t.Fatal(err)
			}
			if err := wb.Pass(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The admin users exist on both sides already (as "admin"): syncing them
	// across must not break either side.
	a.Must(200, &a.Admin, "PUT", "/shared", nil, map[string]string{"x-amz-bucket-object-lock-enabled": "true"})
	a.Must(200, &a.Admin, "PUT", "/shared/from-a.txt", []byte("A"), nil)
	b.Must(200, &b.Admin, "PUT", "/only-b", nil, nil)
	b.Must(200, &b.Admin, "PUT", "/only-b/from-b.txt", []byte("B"), nil)
	settle()
	b.Must(200, &b.Admin, "PUT", "/shared/from-b.txt", []byte("B2"), nil)
	settle()

	for _, c := range []struct {
		n      *testutil.Node
		path   string
		expect string
	}{{b, "/shared/from-a.txt", "A"}, {a, "/only-b/from-b.txt", "B"}, {a, "/shared/from-b.txt", "B2"}} {
		resp, body := c.n.Call(&c.n.Admin, "GET", c.path, nil, nil)
		c.n.Expect(200, resp, body)
		if string(body) != c.expect {
			t.Fatalf("%s: %q", c.path, body)
		}
	}
	// No echo: each object has exactly one version on each side.
	for _, n := range []*testutil.Node{a, b} {
		resp, body := n.Call(&n.Admin, "GET", "/shared?versions", nil, nil)
		n.Expect(200, resp, body)
		if c := strings.Count(string(body), "<Version>"); c != 2 {
			t.Fatalf("expected 2 versions in shared on %s, got %d: %s", n.URL(), c, body)
		}
	}
	// The locked bucket arrived locked and versioned on B.
	resp, body := b.Call(&b.Admin, "GET", "/shared?object-lock", nil, nil)
	b.Expect(200, resp, body)
	if !strings.Contains(string(body), "Enabled") {
		t.Fatalf("lock not replicated: %s", body)
	}
	resp, body = b.Call(&b.Admin, "GET", "/shared?versioning", nil, nil)
	b.Expect(200, resp, body)
	if !strings.Contains(string(body), "Enabled") {
		t.Fatalf("versioning not replicated: %s", body)
	}
	// A user created on A, with a password and a key, appears on B and works there.
	creds := a.CreateUser("carol", meta.RoleReadWrite, nil)
	cu, _ := a.Core.DB.GetUserByName(ctx, "carol")
	if _, err := a.Core.Propose(ctx, &meta.SetUserPassword{ID: cu.ID, Hash: "$2a$10$fakehashfakehashfakehashfakehashfakehashfakehashfakeha"}); err != nil {
		t.Fatal(err)
	}
	settle()
	bu, err := b.Core.DB.GetUserByName(ctx, "carol")
	if err != nil || bu.Role != meta.RoleReadWrite || bu.PasswordHash == "" {
		t.Fatalf("carol on B: %+v %v", bu, err)
	}
	b.Must(200, &creds, "PUT", "/only-b/by-carol.txt", []byte("c"), nil)
	// And the mirrored user does not bounce back as a change on A: still one carol, same id.
	au, _ := a.Core.DB.GetUserByName(ctx, "carol")
	if au.ID != cu.ID {
		t.Fatal("carol changed on A")
	}
	// Logs are trimmed on both sides once everything has been pushed.
	settle()
	for _, n := range []*testutil.Node{a, b} {
		if cnt, _, _, _ := n.Core.DB.ReplicationLogStats(ctx); cnt != 0 {
			t.Fatalf("log not trimmed on %s: %d", n.URL(), cnt)
		}
	}
}

// TestWriteAck checks synchronous acknowledgement: with write_ack=one a put
// on A is on B before A answers, replay does not duplicate it, and when B is
// unreachable the put is refused (and never committed) until the level is
// local again.
func TestWriteAck(t *testing.T) {
	a := testutil.Start(t)
	b := testutil.Start(t)
	ctx := context.Background()
	bFull := full(t, b)
	rule := replication.Rule{Targets: []replication.Target{{Name: "cellar:b", Enabled: true, Cellar: true, Endpoint: bFull.URL, AccessKey: b.Admin.AccessKeyID, SecretKey: b.Admin.SecretKey, PathStyle: true}}}
	raw, _ := json.Marshal(rule)
	if _, err := a.Core.Propose(ctx, &meta.SetSetting{Key: replication.GlobalKey, Value: string(raw)}); err != nil {
		t.Fatal(err)
	}
	level := "one"
	w := replication.New(replication.Deps{DB: a.Core.DB, Propose: a.Core.ProposeRaw, Chunks: a.Core.Chunks, Log: slog.Default(),
		ServerID: func() string { return "s-a" }, WriteAck: func(context.Context) string { return level }}, time.Hour)
	a.API.SetSync(w)

	a.Must(200, &a.Admin, "PUT", "/data", nil, nil)
	a.Must(200, &a.Admin, "PUT", "/data?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	a.Must(200, &a.Admin, "PUT", "/data/sync.txt", []byte("hello"), map[string]string{"x-amz-meta-k": "v"})
	// Already on B, without any replication pass, with the same version id.
	resp, body := b.Call(&b.Admin, "GET", "/data/sync.txt", nil, nil)
	b.Expect(200, resp, body)
	av, _ := a.Core.DB.GetObject(ctx, "data", "sync.txt", "")
	if string(body) != "hello" || resp.Header.Get("X-Amz-Meta-K") != "v" || resp.Header.Get("X-Amz-Version-Id") != av.VersionID {
		t.Fatalf("sync push: %q %v (a vid %s)", body, resp.Header, av.VersionID)
	}
	// The log replay skips what was pushed synchronously: still one version on B.
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	resp, body = b.Call(&b.Admin, "GET", "/data?versions", nil, nil)
	b.Expect(200, resp, body)
	if c := strings.Count(string(body), "<Version>"); c != 1 {
		t.Fatalf("expected 1 version on B after replay, got %d", c)
	}
	// Deletes are acknowledged the same way: the marker shows up on B at once.
	a.Must(204, &a.Admin, "DELETE", "/data/sync.txt", nil, nil)
	resp, _ = b.Call(&b.Admin, "GET", "/data/sync.txt", nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("marker not pushed synchronously: %d", resp.StatusCode)
	}

	// B goes away: writes are refused and not committed locally.
	bFull.Close()
	resp, body = a.Call(&a.Admin, "PUT", "/data/lost.txt", []byte("x"), nil)
	if resp.StatusCode != 503 || !strings.Contains(string(body), "acknowledged") {
		t.Fatalf("expected 503 while B is down, got %d %s", resp.StatusCode, body)
	}
	if _, err := a.Core.DB.GetObject(ctx, "data", "lost.txt", ""); err == nil {
		t.Fatal("refused write must not be committed")
	}
	// Reads still work, and local acknowledgement lets writes through.
	a.Must(404, &a.Admin, "GET", "/data/sync.txt", nil, nil)
	level = "local"
	a.Must(200, &a.Admin, "PUT", "/data/local.txt", []byte("y"), nil)
	if st := w.Status(ctx); len(st) == 0 {
		t.Fatal("status should list the cellar target")
	}
}

// TestObjectLockAcrossServers: retention and legal hold set on A protect the
// same version on B, follow later changes, and block deletion on both.
func TestObjectLockAcrossServers(t *testing.T) {
	a := testutil.Start(t)
	b := testutil.Start(t)
	ctx := context.Background()
	bFull := full(t, b)
	rule := replication.Rule{Targets: []replication.Target{{Name: "peer", Enabled: true, Endpoint: bFull.URL, AccessKey: b.Admin.AccessKeyID, SecretKey: b.Admin.SecretKey, PathStyle: true}}}
	raw, _ := json.Marshal(rule)
	if _, err := a.Core.Propose(ctx, &meta.SetSetting{Key: replication.GlobalKey, Value: string(raw)}); err != nil {
		t.Fatal(err)
	}
	w := replication.New(replication.Deps{DB: a.Core.DB, Propose: a.Core.ProposeRaw, Chunks: a.Core.Chunks, Log: slog.Default(), ServerID: func() string { return "s-a" }}, time.Hour)
	a.Must(200, &a.Admin, "PUT", "/vault", nil, map[string]string{"x-amz-bucket-object-lock-enabled": "true"})
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	a.Must(200, &a.Admin, "PUT", "/vault/doc", []byte("d"), map[string]string{"x-amz-object-lock-mode": "GOVERNANCE", "x-amz-object-lock-retain-until-date": until, "x-amz-object-lock-legal-hold": "ON"})
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	v, _ := a.Core.DB.GetObject(ctx, "vault", "doc", "")
	resp, body := b.Call(&b.Admin, "GET", "/vault/doc?retention&versionId="+v.VersionID, nil, nil)
	b.Expect(200, resp, body)
	if !strings.Contains(string(body), "GOVERNANCE") || !strings.Contains(string(body), until) {
		t.Fatalf("retention on B: %s", body)
	}
	resp, body = b.Call(&b.Admin, "GET", "/vault/doc?legal-hold&versionId="+v.VersionID, nil, nil)
	b.Expect(200, resp, body)
	if !strings.Contains(string(body), "ON") {
		t.Fatalf("legal hold on B: %s", body)
	}
	// Locked on both sides: a versioned delete is refused on B as on A.
	if r, _ := b.Call(&b.Admin, "DELETE", "/vault/doc?versionId="+v.VersionID, nil, nil); r.StatusCode != 403 {
		t.Fatalf("B let a held version go: %d", r.StatusCode)
	}
	// Release the hold on A: B follows, and governance can then be bypassed there too.
	a.Must(200, &a.Admin, "PUT", "/vault/doc?legal-hold&versionId="+v.VersionID, []byte("<LegalHold><Status>OFF</Status></LegalHold>"), nil)
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	resp, body = b.Call(&b.Admin, "GET", "/vault/doc?legal-hold&versionId="+v.VersionID, nil, nil)
	b.Expect(200, resp, body)
	if !strings.Contains(string(body), "OFF") {
		t.Fatalf("hold release did not reach B: %s", body)
	}
	if r, _ := b.Call(&b.Admin, "DELETE", "/vault/doc?versionId="+v.VersionID, nil, map[string]string{"x-amz-bypass-governance-retention": "true"}); r.StatusCode != 204 {
		t.Fatalf("governance bypass on B: %d", r.StatusCode)
	}
}
