package cellar_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/cellar"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/replication"
	"github.com/enetforge/hogshead/internal/s3client"
	"github.com/enetforge/hogshead/internal/testutil"
	"github.com/enetforge/hogshead/internal/usage"
)

// node is a full server: S3 API with write acknowledgement, admin API,
// public join endpoints, replication worker and cellar manager, as wire
// builds it, on one httptest address.
type node struct {
	n    *testutil.Node
	srv  *httptest.Server
	repl *replication.Worker
	cel  *cellar.Manager
	id   string
	name string
}

func start(t *testing.T, id, name string) *node {
	t.Helper()
	n := testutil.Start(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	repl := replication.New(replication.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Log: log,
		ServerID: func() string { return id },
		WriteAck: func(ctx context.Context) string { v, _ := n.Core.DB.GetSetting(ctx, cellar.KeyWriteAck); return v },
	}, time.Hour)
	n.API.SetSync(repl)
	n.API.SetOnChange(repl.Trigger)
	nd := &node{n: n, repl: repl, id: id, name: name}
	nd.cel = cellar.New(cellar.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Worker: repl, Version: "test", Log: log,
		ServerID: func() string { return id }, Name: func() string { return name },
		Endpoint: func() string { return nd.srv.URL }})
	adm := admin.New(admin.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Cfg: n.Core.Cfg, Log: log, Started: time.Now(),
		Replication: repl, Cellar: nd.cel, ServerID: func() string { return id }, Name: func() string { return name }})
	mux := http.NewServeMux()
	mux.Handle(admin.Prefix+"/", adm)
	mux.Handle(cellar.PublicPrefix+"/", nd.cel.PublicHandler())
	mux.Handle("/", n.API)
	nd.srv = httptest.NewServer(mux)
	t.Cleanup(nd.srv.Close)
	return nd
}

// tick runs one round of everything a running server would do on its own.
func tick(ctx context.Context, nodes ...*node) {
	for _, nd := range nodes {
		nd.cel.Step(ctx)
	}
	for _, nd := range nodes {
		_ = nd.repl.Pass(ctx)
	}
}

func waitFor(t *testing.T, what string, cond func() bool, dump ...func() string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, d := range dump {
		t.Log(d())
	}
	t.Fatalf("timed out waiting for %s", what)
}

// describe renders a node's cellar status and replication targets for failures.
func describe(ctx context.Context, nd *node) string {
	st, _ := nd.cel.Status(ctx)
	g, _, _ := replication.LoadGlobal(ctx, nd.n.Core.DB)
	var b strings.Builder
	fmt.Fprintf(&b, "%s: active=%v members=%d pending=%d joining=%v\n", nd.name, st.Active, len(st.Members), len(st.Pending), st.Joining)
	for _, m := range st.Members {
		fmt.Fprintf(&b, "  member %s self=%v joining=%v lag=%d err=%q backfilled=%v backfilling=%v\n", m.Name, m.Self, m.Joining, m.Lag, m.LastError, m.Backfilled, m.Backfilling)
	}
	for _, t := range g.Targets {
		fmt.Fprintf(&b, "  target %s -> %s enabled=%v cellar=%v\n", t.Name, t.Endpoint, t.Enabled, t.Cellar)
	}
	for _, r := range nd.repl.Status(ctx) {
		fmt.Fprintf(&b, "  repl %q -> %s lag=%d pushed=%d err=%q\n", r.Bucket, r.Target.Name, r.Lag, r.Pushed, r.LastError)
	}
	all, _ := nd.n.Core.DB.AllSettings(ctx)
	for k := range all {
		if strings.HasPrefix(k, "cellar.approved:") || strings.HasPrefix(k, "cellar.pending:") {
			fmt.Fprintf(&b, "  setting %s\n", k)
		}
	}
	return b.String()
}

func TestCellarLifecycle(t *testing.T) {
	ctx := context.Background()
	a := start(t, "s-a", "hogshead1")
	b := start(t, "s-b", "hogshead2")
	c := start(t, "s-c", "hogshead3")

	// A has data before the cellar exists.
	a.n.Must(200, &a.n.Admin, "PUT", "/photos", nil, map[string]string{"x-amz-bucket-object-lock-enabled": "true"})
	a.n.Must(200, &a.n.Admin, "PUT", "/photos/old.jpg", []byte("old"), map[string]string{"Content-Type": "image/jpeg"})
	alice := a.n.CreateUser("alice", meta.RoleReadWrite, nil)

	st, err := a.cel.Init(ctx, "office", "")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Active || st.Name != "office" || st.WriteAck != "one" || len(st.Members) != 1 || !st.Members[0].Self {
		t.Fatalf("init: %+v", st)
	}
	if st.Pending == nil || st.Members == nil {
		t.Fatal("status lists must never be nil (they become null in JSON and break the UI)")
	}
	if _, err := a.cel.Init(ctx, "again", ""); err == nil {
		t.Fatal("double init accepted")
	}

	// B asks to join; nothing happens until A approves.
	if _, err := b.cel.Join(ctx, a.srv.URL, "", false); err != nil {
		t.Fatal(err)
	}
	st, _ = a.cel.Status(ctx)
	if len(st.Pending) != 1 || st.Pending[0].ID != "s-b" || st.Pending[0].Name != "hogshead2" || st.Pending[0].Endpoint != b.srv.URL {
		t.Fatalf("pending: %+v", st.Pending)
	}
	tick(ctx, a, b)
	if bst, _ := b.cel.Status(ctx); bst.Active || bst.Joining == nil {
		t.Fatalf("B joined without approval: %+v", bst)
	}
	// Rejecting then re-requesting works; then approve.
	if err := a.cel.Reject(ctx, "s-b"); err != nil {
		t.Fatal(err)
	}
	tick(ctx, a, b) // B learns the request is gone
	if bst, _ := b.cel.Status(ctx); bst.Joining == nil || bst.Joining.LastError == "" {
		t.Fatalf("B should see the rejection: %+v", bst.Joining)
	}
	if _, err := b.cel.Join(ctx, a.srv.URL, "", false); err != nil {
		t.Fatal(err)
	}
	if err := a.cel.Approve(ctx, "s-b", "admin"); err != nil {
		t.Fatal(err)
	}
	// Until B collects its approval, A lists it as joining and has no target
	// to it, so nothing can fail against a server that is not yet a member.
	a.cel.Step(ctx)
	ast, _ := a.cel.Status(ctx)
	var bRow *cellar.MemberStatus
	for i := range ast.Members {
		if ast.Members[i].ID == "s-b" {
			bRow = &ast.Members[i]
		}
	}
	if bRow == nil || !bRow.Joining || bRow.LastError != "" {
		t.Fatalf("B should be joining without errors: %+v", bRow)
	}
	if g, _, _ := replication.LoadGlobal(ctx, a.n.Core.DB); len(g.Targets) != 0 {
		t.Fatalf("no target expected before B collected its approval: %+v", g.Targets)
	}
	tick(ctx, a, b) // B polls, completes the join, reconciles
	bst, _ := b.cel.Status(ctx)
	if !bst.Active || bst.ID != st.ID || bst.Name != "office" || len(bst.Members) != 2 {
		t.Fatalf("B after approval: %+v", bst)
	}
	// Both sides have a target at each other and backfill A's data into B.
	waitFor(t, "backfill of B", func() bool {
		tick(ctx, a, b)
		resp, _ := b.n.Call(&b.n.Admin, "GET", "/photos/old.jpg", nil, nil)
		return resp.StatusCode == 200
	})
	resp, body := b.n.Call(&b.n.Admin, "GET", "/photos?object-lock", nil, nil)
	b.n.Expect(200, resp, body)
	if !strings.Contains(string(body), "Enabled") {
		t.Fatalf("lock not carried: %s", body)
	}
	waitFor(t, "users on B", func() bool {
		tick(ctx, a, b)
		_, err := b.n.Core.DB.GetUserByName(ctx, "alice")
		return err == nil
	})
	// alice's key from A works on B (users and keys were synced).
	b.n.Must(200, &alice, "PUT", "/photos/by-alice.jpg", []byte("x"), nil)

	// Write on B with write_ack=one: on A before B answers.
	b.n.Must(200, &b.n.Admin, "PUT", "/photos/from-b.jpg", []byte("b"), nil)
	resp, body = a.n.Call(&a.n.Admin, "GET", "/photos/from-b.jpg", nil, nil)
	a.n.Expect(200, resp, body)
	if string(body) != "b" {
		t.Fatalf("sync write: %q", body)
	}

	// C joins through B; A learns about it via the replicated member list.
	if _, err := c.cel.Join(ctx, b.srv.URL, "", false); err != nil {
		t.Fatal(err)
	}
	if err := b.cel.Approve(ctx, "s-c", "admin"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "C to join and A to see three members", func() bool {
		tick(ctx, a, b, c)
		ast, _ := a.cel.Status(ctx)
		cst, _ := c.cel.Status(ctx)
		return cst.Active && len(ast.Members) == 3 && len(cst.Members) == 3
	})
	waitFor(t, "C to receive everything", func() bool {
		tick(ctx, a, b, c)
		r1, _ := c.n.Call(&c.n.Admin, "GET", "/photos/old.jpg", nil, nil)
		r2, _ := c.n.Call(&c.n.Admin, "GET", "/photos/from-b.jpg", nil, nil)
		return r1.StatusCode == 200 && r2.StatusCode == 200
	})
	// A write on C reaches both A and B exactly once.
	c.n.Must(200, &c.n.Admin, "PUT", "/photos/from-c.jpg", []byte("c"), nil)
	waitFor(t, "C's write everywhere", func() bool {
		tick(ctx, a, b, c)
		r1, _ := a.n.Call(&a.n.Admin, "GET", "/photos/from-c.jpg", nil, nil)
		r2, _ := b.n.Call(&b.n.Admin, "GET", "/photos/from-c.jpg", nil, nil)
		return r1.StatusCode == 200 && r2.StatusCode == 200
	})
	tick(ctx, a, b, c)
	tick(ctx, a, b, c)
	for _, nd := range []*node{a, b, c} {
		resp, body := nd.n.Call(&nd.n.Admin, "GET", "/photos?versions", nil, nil)
		nd.n.Expect(200, resp, body)
		if got := strings.Count(string(body), "<Version>"); got != 4 {
			t.Fatalf("%s: expected 4 versions (old, by-alice, from-b, from-c), got %d: %s", nd.name, got, body)
		}
	}
	// Bucket deletion travels to cellar members, data included.
	c.n.Must(200, &c.n.Admin, "PUT", "/scratch", nil, nil)
	c.n.Must(200, &c.n.Admin, "PUT", "/scratch/x", []byte("x"), nil)
	waitFor(t, "scratch on A", func() bool {
		tick(ctx, a, b, c)
		r, _ := a.n.Call(&a.n.Admin, "GET", "/scratch/x", nil, nil)
		return r.StatusCode == 200
	})
	c.n.Must(204, &c.n.Admin, "DELETE", "/scratch/x", nil, nil)
	c.n.Must(204, &c.n.Admin, "DELETE", "/scratch", nil, nil)
	waitFor(t, "scratch gone from A", func() bool {
		tick(ctx, a, b, c)
		r, _ := a.n.Call(&a.n.Admin, "HEAD", "/scratch", nil, nil)
		return r.StatusCode == 404
	})

	// Readiness: with everything pushed, members report ready.
	tick(ctx, a, b, c)
	ast, _ = a.cel.Status(ctx)
	if ast.SelfID != "s-a" || len(ast.Pending) != 0 {
		t.Fatalf("A status: %+v", ast)
	}

	// Remove C: key rotates, remaining members keep working, C is out.
	oldKey, _ := a.n.Core.DB.GetSetting(ctx, cellar.KeyAccessID)
	if err := a.cel.Remove(ctx, "s-c"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B to see two members and the new key", func() bool {
		tick(ctx, a, b)
		bst, _ := b.cel.Status(ctx)
		k, _ := b.n.Core.DB.GetSetting(ctx, cellar.KeyAccessID)
		return len(bst.Members) == 2 && k != "" && k != oldKey
	})
	a.n.Must(200, &a.n.Admin, "PUT", "/photos/after-removal.jpg", []byte("r"), nil)
	waitFor(t, "write to reach B after rotation", func() bool {
		tick(ctx, a, b)
		r, _ := b.n.Call(&b.n.Admin, "GET", "/photos/after-removal.jpg", nil, nil)
		return r.StatusCode == 200
	})
	if r, _ := c.n.Call(&c.n.Admin, "GET", "/photos/after-removal.jpg", nil, nil); r.StatusCode == 200 {
		t.Fatal("removed member still receives data")
	}

	// B leaves: A is alone, acknowledges locally, and keeps the data.
	if err := b.cel.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	tick(ctx, a, b)
	if bst, _ := b.cel.Status(ctx); bst.Active {
		t.Fatal("B still active after leave")
	}
	waitFor(t, "A alone", func() bool {
		tick(ctx, a)
		ast, _ := a.cel.Status(ctx)
		return len(ast.Members) == 1
	})
	a.n.Must(200, &a.n.Admin, "PUT", "/photos/alone.jpg", []byte("a"), nil)
	resp, body = b.n.Call(&b.n.Admin, "GET", "/photos/from-c.jpg", nil, nil)
	b.n.Expect(200, resp, body)
	if err := a.cel.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if a.cel.Active(ctx) {
		t.Fatal("A still active")
	}
}

// TestJoinMerge covers the non-empty joiner: refused by default, allowed with
// merge, and the clash rules (older member's config and users win, newest
// object wins, joiner-only buckets keep their config, lock mismatch blocks).
func TestJoinMerge(t *testing.T) {
	ctx := context.Background()
	a := start(t, "s-a", "hogshead1")
	b := start(t, "s-b", "hogshead2")
	if _, err := a.cel.Init(ctx, "office", ""); err != nil {
		t.Fatal(err)
	}
	// Both have "shared"; A's has a policy, B's has none and a conflicting object.
	a.n.Must(200, &a.n.Admin, "PUT", "/shared", nil, nil)
	a.n.Must(200, &a.n.Admin, "PUT", "/shared?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	a.n.Must(204, &a.n.Admin, "PUT", "/shared?policy", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::shared/*"}]}`), nil)
	a.n.Must(200, &a.n.Admin, "PUT", "/shared/k", []byte("from-a-older"), nil)
	time.Sleep(20 * time.Millisecond)
	b.n.Must(200, &b.n.Admin, "PUT", "/shared", nil, nil)
	b.n.Must(200, &b.n.Admin, "PUT", "/shared?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	b.n.Must(200, &b.n.Admin, "PUT", "/shared/k", []byte("from-b-newer"), nil)
	b.n.Must(200, &b.n.Admin, "PUT", "/only-b", nil, nil)
	b.n.Must(204, &b.n.Admin, "PUT", "/only-b?policy", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::only-b/*"}]}`), nil)
	b.n.Must(200, &b.n.Admin, "PUT", "/only-b/x", []byte("x"), nil)
	// Same user on both sides with different roles; A (older) must win.
	a.n.CreateUser("carol", meta.RoleReadOnly, nil)
	bkey := b.n.CreateUser("carol", meta.RoleAdmin, nil)
	_, _ = a.n.Core.Propose(ctx, &meta.SetSetting{Key: "uploads.max_age", Value: "24h"})
	_, _ = b.n.Core.Propose(ctx, &meta.SetSetting{Key: "uploads.max_age", Value: "1h"})

	// Refused without merge, with the bucket names in the message.
	_, err := b.cel.Join(ctx, a.srv.URL, "", false)
	var ne *cellar.ErrNotEmpty
	if !errors.As(err, &ne) || len(ne.Buckets) != 2 {
		t.Fatalf("expected not-empty refusal, got %v", err)
	}
	if _, err := b.cel.Join(ctx, a.srv.URL, "", true); err != nil {
		t.Fatal(err)
	}
	st, _ := a.cel.Status(ctx)
	if len(st.Pending) != 1 || st.Pending[0].Buckets != 2 {
		t.Fatalf("approver should see the merge: %+v", st.Pending)
	}
	if err := a.cel.Approve(ctx, "s-b", "admin"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "merge to settle", func() bool {
		tick(ctx, a, b)
		bst, _ := b.cel.Status(ctx)
		if !bst.Active {
			return false
		}
		r1, _ := a.n.Call(&a.n.Admin, "GET", "/only-b/x", nil, nil)
		r2, _ := b.n.Call(&b.n.Admin, "GET", "/shared?policy", nil, nil)
		if r1.StatusCode != 200 || r2.StatusCode != 200 {
			return false
		}
		// Both backfills (A->B and B->A) have landed once each side holds both versions of k.
		for _, nd := range []*node{a, b} {
			_, body := nd.n.Call(&nd.n.Admin, "GET", "/shared?versions", nil, nil)
			if strings.Count(string(body), "<Version>") != 2 {
				return false
			}
		}
		return true
	}, func() string { return describe(ctx, a) + describe(ctx, b) })
	tick(ctx, a, b)
	tick(ctx, a, b)
	// Newest object wins on both sides, both versions kept.
	for _, nd := range []*node{a, b} {
		resp, body := nd.n.Call(&nd.n.Admin, "GET", "/shared/k", nil, nil)
		nd.n.Expect(200, resp, body)
		if string(body) != "from-b-newer" {
			t.Fatalf("%s: latest %q", nd.name, body)
		}
		resp, body = nd.n.Call(&nd.n.Admin, "GET", "/shared?versions", nil, nil)
		nd.n.Expect(200, resp, body)
		if c := strings.Count(string(body), "<Version>"); c != 2 {
			t.Fatalf("%s: %d versions", nd.name, c)
		}
	}
	// A's (older member) policy on the shared bucket reached B; B's only-b policy reached A.
	resp, body := b.n.Call(&b.n.Admin, "GET", "/shared?policy", nil, nil)
	b.n.Expect(200, resp, body)
	if !strings.Contains(string(body), "arn:aws:s3:::shared/*") {
		t.Fatalf("cellar policy did not win: %s", body)
	}
	resp, body = a.n.Call(&a.n.Admin, "GET", "/only-b?policy", nil, nil)
	a.n.Expect(200, resp, body)
	// Users: A's carol wins; B's key for carol still works on both.
	waitFor(t, "carol settled", func() bool {
		tick(ctx, a, b)
		ub, err := b.n.Core.DB.GetUserByName(ctx, "carol")
		return err == nil && ub.Role == meta.RoleReadOnly
	})
	ua, _ := a.n.Core.DB.GetUserByName(ctx, "carol")
	if ua.Role != meta.RoleReadOnly {
		t.Fatalf("joiner overwrote the cellar's user: %+v", ua)
	}
	waitFor(t, "B's key for carol on A", func() bool {
		tick(ctx, a, b)
		_, err := a.n.Core.DB.LookupAccessKey(ctx, bkey.AccessKeyID)
		return err == nil
	})
	// Settings: the cellar's value wins.
	if v, _ := b.n.Core.DB.GetSetting(ctx, "uploads.max_age"); v != "24h" {
		t.Fatalf("settings: B has %q", v)
	}
	if v, _ := a.n.Core.DB.GetSetting(ctx, "uploads.max_age"); v != "24h" {
		t.Fatalf("settings: A has %q", v)
	}

	// A lock mismatch blocks the merge with a clear error.
	c := start(t, "s-c", "hogshead3")
	c.n.Must(200, &c.n.Admin, "PUT", "/shared", nil, map[string]string{"x-amz-bucket-object-lock-enabled": "true"})
	if _, err := c.cel.Join(ctx, a.srv.URL, "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.cel.Approve(ctx, "s-c", "admin"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "lock clash reported", func() bool {
		tick(ctx, a, b, c)
		cst, _ := c.cel.Status(ctx)
		return !cst.Active && cst.Joining != nil && strings.Contains(cst.Joining.LastError, "Object Lock differs on shared")
	})
}

// TestManualReplicationOverlap covers manual targets that collide with a
// cellar: a join is refused while the joiner replicates to the member it
// asks, a member's manual target at a newcomer is paused with a notice, new
// manual targets at members are refused, and the service account and its
// keys cannot be touched by hand.
func TestManualReplicationOverlap(t *testing.T) {
	ctx := context.Background()
	a := start(t, "s-a", "hogshead1")
	b := start(t, "s-b", "hogshead2")
	if _, err := a.cel.Init(ctx, "office", ""); err != nil {
		t.Fatal(err)
	}
	manual := func(nd *node, name, endpoint string) {
		rule := replication.Rule{Targets: []replication.Target{{Name: name, Enabled: true, Endpoint: endpoint, AccessKey: "a", SecretKey: "s", PathStyle: true, SyncConfig: true}}}
		raw, _ := json.Marshal(rule)
		if _, err := nd.n.Core.Propose(ctx, &meta.SetSetting{Key: replication.GlobalKey, Value: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	// B replicates to A by hand: joining A is refused and says so.
	manual(b, "to-a", a.srv.URL)
	_, err := b.cel.Join(ctx, a.srv.URL, "", false)
	if err == nil || !strings.Contains(err.Error(), `server-wide target "to-a"`) {
		t.Fatalf("expected overlap refusal, got %v", err)
	}
	if _, err := b.n.Core.Propose(ctx, &meta.SetSetting{Key: replication.GlobalKey, Value: ""}); err != nil {
		t.Fatal(err)
	}
	// A replicates to B by hand; after B joins, A pauses that target and says why.
	manual(a, "to-b", b.srv.URL)
	if _, err := b.cel.Join(ctx, a.srv.URL, "", false); err != nil {
		t.Fatal(err)
	}
	if err := a.cel.Approve(ctx, "s-b", "admin"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B to join and A to pause its manual target", func() bool {
		tick(ctx, a, b)
		g, _, _ := replication.LoadGlobal(ctx, a.n.Core.DB)
		for _, tg := range g.Targets {
			if tg.Name == "to-b" {
				return !tg.Enabled
			}
		}
		return false
	}, func() string { return describe(ctx, a) + describe(ctx, b) })
	ast, _ := a.cel.Status(ctx)
	if len(ast.Notices) == 0 || !strings.Contains(ast.Notices[0].Message, `paused server-wide target "to-b"`) {
		t.Fatalf("expected a notice, got %+v", ast.Notices)
	}
	// Adding a manual target at a member is refused, on both sides.
	adminB := s3client.New(s3client.Endpoint{Endpoint: b.srv.URL, AccessKey: b.n.Admin.AccessKeyID, SecretKey: b.n.Admin.SecretKey, PathStyle: true})
	err = adminB.AdminJSON(ctx, "PUT", "/replication/global", map[string]any{"endpoint": a.srv.URL, "access_key": "x", "secret_key": "y"}, nil)
	var se *s3client.Error
	if !errors.As(err, &se) || se.Status != 409 || !strings.Contains(se.Body, "CellarMember") {
		t.Fatalf("manual target at a member should be refused: %v", err)
	}
	b.n.Must(200, &b.n.Admin, "PUT", "/data", nil, nil)
	err = adminB.AdminJSON(ctx, "PUT", "/buckets/data/replication", map[string]any{"endpoint": a.srv.URL, "bucket": "data", "access_key": "x", "secret_key": "y"}, nil)
	if !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("bucket target at a member should be refused: %v", err)
	}
	// The service account and its key are protected from manual changes.
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{"DELETE", "/users/cellar", nil},
		{"PATCH", "/users/cellar", map[string]any{"disabled": true}},
		{"PUT", "/users/cellar/password", map[string]string{"password": "longenough1"}},
	} {
		err := adminB.AdminJSON(ctx, call.method, call.path, call.body, nil)
		if !errors.As(err, &se) || se.Status != 409 || !strings.Contains(se.Body, "CellarManaged") {
			t.Fatalf("%s %s should be refused: %v", call.method, call.path, err)
		}
	}
	keyID, _ := b.n.Core.DB.GetSetting(ctx, cellar.KeyAccessID)
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{"DELETE", "/keys/" + keyID, nil},
		{"PATCH", "/keys/" + keyID, map[string]any{"disabled": true}},
	} {
		err := adminB.AdminJSON(ctx, call.method, call.path, call.body, nil)
		if !errors.As(err, &se) || se.Status != 409 {
			t.Fatalf("%s %s should be refused: %v", call.method, call.path, err)
		}
	}
	// Creating or importing a user named "cellar" by hand is refused.
	err = adminB.AdminJSON(ctx, "POST", "/users", map[string]any{"name": "cellar", "role": "read-only"}, nil)
	if !errors.As(err, &se) || se.Status != 409 || !strings.Contains(se.Body, "ReservedUser") {
		t.Fatalf("creating the reserved user should be refused: %v", err)
	}
	c := start(t, "s-c", "hogshead3")
	adminC := s3client.New(s3client.Endpoint{Endpoint: c.srv.URL, AccessKey: c.n.Admin.AccessKeyID, SecretKey: c.n.Admin.SecretKey, PathStyle: true})
	err = adminC.AdminJSON(ctx, "POST", "/users", map[string]any{"name": "cellar", "role": "admin"}, nil)
	if !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("reserved outside a cellar too: %v", err)
	}
	err = adminC.AdminJSON(ctx, "PUT", "/users/cellar", map[string]any{"role": "admin", "buckets": []string{}, "disabled": false}, nil)
	if !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("import outside a cellar is refused: %v", err)
	}
	// Config sync between members still manages the account (import routes).
	syncB := s3client.New(s3client.Endpoint{Endpoint: b.srv.URL, AccessKey: b.n.Admin.AccessKeyID, SecretKey: b.n.Admin.SecretKey, PathStyle: true}).WithOrigin("s-a")
	if err := syncB.AdminJSON(ctx, "PUT", "/users/cellar", map[string]any{"role": "admin", "buckets": []string{}, "disabled": false}, nil); err != nil {
		t.Fatalf("import of the service account must stay allowed for sync: %v", err)
	}
}

// TestInitAdoptsLegacyServiceUser: a pre-existing user named "cellar" (from
// before the name was reserved) becomes a clean service account on init.
func TestInitAdoptsLegacyServiceUser(t *testing.T) {
	ctx := context.Background()
	a := start(t, "s-a", "hogshead1")
	a.n.CreateUser("cellar", meta.RoleReadOnly, []string{"x"})
	u, _ := a.n.Core.DB.GetUserByName(ctx, "cellar")
	_, _ = a.n.Core.Propose(ctx, &meta.SetUserPassword{ID: u.ID, Hash: "$2a$10$x"})
	if _, err := a.cel.Init(ctx, "office", ""); err != nil {
		t.Fatal(err)
	}
	u, _ = a.n.Core.DB.GetUserByName(ctx, "cellar")
	if u.Role != meta.RoleAdmin || len(u.Buckets) != 0 || u.Disabled || u.PasswordHash != "" {
		t.Fatalf("legacy account not normalised: %+v", u)
	}
}

// TestCellarUsage: cellar-scoped usage sums every member and can break down
// by member; an unreachable member is reported and left out.
func TestCellarUsage(t *testing.T) {
	ctx := context.Background()
	a := start(t, "s-a", "hogshead1")
	b := start(t, "s-b", "hogshead2")
	if _, err := a.cel.Init(ctx, "office", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.cel.Join(ctx, a.srv.URL, "", false); err != nil {
		t.Fatal(err)
	}
	if err := a.cel.Approve(ctx, "s-b", "admin"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B to join", func() bool { tick(ctx, a, b); st, _ := b.cel.Status(ctx); return st.Active })
	a.n.Must(200, &a.n.Admin, "PUT", "/data", nil, nil)
	waitFor(t, "data on B", func() bool {
		tick(ctx, a, b)
		r, _ := b.n.Call(&b.n.Admin, "HEAD", "/data", nil, nil)
		return r.StatusCode == 200
	})
	// Count only from here on, so the setup traffic above stays out.
	ua, ub := usage.New(a.n.Core.ProposeRaw, nil), usage.New(b.n.Core.ProposeRaw, nil)
	a.n.API.SetAccess(ua.Handle)
	b.n.API.SetAccess(ub.Handle)
	a.n.Must(200, &a.n.Admin, "GET", "/data?list-type=2", nil, nil)
	a.n.Must(200, &a.n.Admin, "GET", "/data?list-type=2", nil, nil)
	b.n.Must(200, &b.n.Admin, "GET", "/data?list-type=2", nil, nil)
	b.n.Must(404, &b.n.Admin, "GET", "/data/nope", nil, nil)
	_ = ua.Flush(ctx)
	_ = ub.Flush(ctx)
	adminA := s3client.New(s3client.Endpoint{Endpoint: a.srv.URL, AccessKey: a.n.Admin.AccessKeyID, SecretKey: a.n.Admin.SecretKey, PathStyle: true})
	var res admin.UsageResponse
	// Filter by user so the cellar's own replication traffic (class
	// "replication", user "cellar") stays out of the arithmetic.
	if err := adminA.AdminJSON(ctx, "GET", "/usage?scope=cellar&by=class&bucket=data&user=admin", nil, &res); err != nil {
		t.Fatal(err)
	}
	if res.Scope != "cellar" || res.Totals.Requests != 4 || res.Totals.Errors != 1 || len(res.Members) != 2 || !res.Members[0].OK || !res.Members[1].OK {
		t.Fatalf("cellar usage: %+v", res)
	}
	byClass := map[string]int64{}
	for _, r := range res.Rows {
		byClass[r.Class] = r.Requests
	}
	if byClass["list"] != 3 || byClass["read"] != 1 {
		t.Fatalf("merged rows: %+v", res.Rows)
	}
	if err := adminA.AdminJSON(ctx, "GET", "/usage?scope=cellar&by=member&bucket=data&user=admin", nil, &res); err != nil {
		t.Fatal(err)
	}
	perMember := map[string]int64{}
	for _, r := range res.Rows {
		perMember[r.Member] = r.Requests
	}
	if perMember["hogshead1"] != 2 || perMember["hogshead2"] != 2 {
		t.Fatalf("by member: %+v", res.Rows)
	}
	// Local scope is just this server.
	if err := adminA.AdminJSON(ctx, "GET", "/usage?by=class&bucket=data&user=admin", nil, &res); err != nil || res.Scope != "local" || res.Totals.Requests != 2 {
		t.Fatalf("local usage: %+v %v", res, err)
	}
	// Replication traffic is visible under its own class, never under the user's.
	a.n.Must(200, &a.n.Admin, "PUT", "/data/pushed.txt", []byte("p"), nil) // write_ack pushes it to B at once
	_ = ub.Flush(ctx)
	adminB := s3client.New(s3client.Endpoint{Endpoint: b.srv.URL, AccessKey: b.n.Admin.AccessKeyID, SecretKey: b.n.Admin.SecretKey, PathStyle: true})
	if err := adminB.AdminJSON(ctx, "GET", "/usage?by=class", nil, &res); err != nil {
		t.Fatal(err)
	}
	seenRepl := false
	for _, r := range res.Rows {
		if r.Class == usage.ClassReplication {
			seenRepl = true
		}
	}
	if !seenRepl {
		t.Fatalf("expected a replication class row: %+v", res.Rows)
	}
	// A dead member is reported, not fatal.
	b.srv.Close()
	if err := adminA.AdminJSON(ctx, "GET", "/usage?scope=cellar&bucket=data&user=admin", nil, &res); err != nil {
		t.Fatal(err)
	}
	if res.Totals.Requests != 2 {
		t.Fatalf("dead member must be left out of the sums: %+v", res.Totals)
	}
	if len(res.Members) != 2 || res.Members[1].OK || res.Members[1].Error == "" {
		t.Fatalf("dead member: %+v", res)
	}
}

// An upgraded server replaces the version recorded when it joined.
func TestMemberVersionRefreshedAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	a := start(t, "s-a", "hogshead1")
	if _, err := a.cel.Init(ctx, "office", ""); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upgraded := cellar.New(cellar.Deps{DB: a.n.Core.DB, Propose: a.n.Core.ProposeRaw, Worker: a.repl, Version: "next", Log: log,
		ServerID: func() string { return a.id }, Name: func() string { return a.name },
		Endpoint: func() string { return a.srv.URL }})
	upgraded.Step(ctx)
	members, err := upgraded.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Version != "next" {
		t.Fatalf("members after upgrade: %+v", members)
	}
}
