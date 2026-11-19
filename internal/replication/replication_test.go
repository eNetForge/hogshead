package replication

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/s3client"
	"github.com/enetforge/hogshead/internal/testutil"
)

// fakePusher records calls.
type fakePusher struct {
	mu   sync.Mutex
	puts map[string]string
	dels []string
	fail bool

	ensured, ensuredVersioned, ensuredLocked int
	config                                   map[string]string
	admin                                    []string
	purged                                   int
	hdrs                                     map[string]map[string]string
	subs                                     []string
}

func (f *fakePusher) PutObject(_ context.Context, key string, body io.Reader, _ int64, headers map[string]string) error {
	if f.fail {
		return &Error{Status: 500, Body: "boom"}
	}
	b, _ := io.ReadAll(body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts[key] = string(b) + "|" + headers["content-type"] + "|" + headers["x-amz-meta-k"]
	if f.hdrs == nil {
		f.hdrs = map[string]map[string]string{}
	}
	cp := map[string]string{}
	for k, v := range headers {
		cp[k] = v
	}
	f.hdrs[key] = cp
	return nil
}
func (f *fakePusher) PutObjectSubresource(_ context.Context, key, versionID, sub, body string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, "PUT "+key+"@"+versionID+"?"+sub+" "+body)
	return nil
}
func (f *fakePusher) DeleteObjectSubresource(_ context.Context, key, versionID, sub string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, "DELETE "+key+"@"+versionID+"?"+sub)
	return nil
}
func (f *fakePusher) DeleteObject(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dels = append(f.dels, key)
	return nil
}
func (f *fakePusher) DeleteObjectVersion(_ context.Context, key, versionID string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if versionID != "" {
		key += "@" + versionID
	}
	f.dels = append(f.dels, key)
	return nil
}
func (f *fakePusher) EnsureBucket(_ context.Context, o s3client.BucketOptions) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured++
	if o.Versioned || o.ObjectLock {
		f.ensuredVersioned++
	}
	if o.ObjectLock {
		f.ensuredLocked++
	}
	return false, nil
}
func (f *fakePusher) PutBucketConfig(_ context.Context, sub, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.config == nil {
		f.config = map[string]string{}
	}
	f.config[sub] = body
	return nil
}
func (f *fakePusher) DeleteBucketConfig(_ context.Context, sub string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.config == nil {
		f.config = map[string]string{}
	}
	f.config[sub] = "<deleted>"
	return nil
}
func (f *fakePusher) HeadObjectVersion(context.Context, string, string) (*s3client.ObjectInfo, error) {
	return nil, s3client.ErrNotFound
}
func (f *fakePusher) PurgeBucket(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purged++
	return nil
}
func (f *fakePusher) AdminJSON(_ context.Context, method, path string, in, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(in)
	f.admin = append(f.admin, method+" "+path+" "+string(b))
	return nil
}

func TestWorkerPushesLog(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	ctx := context.Background()
	fp := &fakePusher{puts: map[string]string{}}
	w := New(Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks,
		Log: slog.Default(), NewClient: func(Target) Pusher { return fp }}, time.Hour)
	n.Must(200, a, "PUT", "/src", nil, nil)
	n.Must(200, a, "PUT", "/src/a.txt", []byte("alpha"), map[string]string{"Content-Type": "text/plain", "x-amz-meta-k": "v"})
	n.Must(200, a, "PUT", "/src/skip/b.txt", []byte("beta"), nil)
	// No rule yet: a pass trims the log entirely.
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if cnt, _, _, _ := n.Core.DB.ReplicationLogStats(ctx); cnt != 0 {
		t.Fatalf("log should be trimmed without rules, has %d", cnt)
	}
	rule := Rule{Targets: []Target{{Name: "dest", Enabled: true, Endpoint: "http://dest:9000", Bucket: "mirror", AccessKey: "a", SecretKey: "s"}}}
	if err := rule.Validate(); err != nil {
		t.Fatal(err)
	}
	if (Target{Name: "a b", Endpoint: "http://x", Bucket: "b", AccessKey: "a", SecretKey: "s"}).Validate() == nil || (Target{Name: "x", Endpoint: "ftp://x"}).Validate() == nil || (Target{Name: "x", Endpoint: "http://x"}).Validate() == nil || (Target{Endpoint: "http://x", Bucket: "b", AccessKey: "a", SecretKey: "s"}).Validate() == nil {
		t.Fatal("validation")
	}
	dup := Rule{Targets: []Target{rule.Targets[0], rule.Targets[0]}}
	if dup.Validate() == nil {
		t.Fatal("duplicate names must be rejected")
	}
	raw, _ := json.Marshal(rule)
	if _, err := n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "src", Value: string(raw)}); err != nil {
		t.Fatal(err)
	}
	// Writes after the rule exists are pushed in order.
	n.Must(200, a, "PUT", "/src/c.txt", []byte("gamma"), map[string]string{"Content-Type": "text/plain", "x-amz-meta-k": "v"})
	n.Must(200, a, "PUT", "/src/c.txt", []byte("gamma2"), nil) // overwrite: delete+put in the log, only the put is pushed
	n.Must(204, a, "DELETE", "/src/c.txt", nil, nil)
	n.Must(200, a, "PUT", "/src/d.txt", []byte("delta"), nil)
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	// c.txt was deleted before the pass ran, so its puts are skipped and the
	// destination only sees the delete; d.txt arrives with its content.
	if fp.puts["d.txt"] != "delta||" || fp.puts["c.txt"] != "" || len(fp.dels) == 0 || fp.dels[0] != "c.txt" {
		t.Fatalf("pushed: %+v dels %v", fp.puts, fp.dels)
	}
	fp.mu.Unlock()
	st := w.Status(ctx)
	if len(st) != 1 || st[0].Lag != 0 || st[0].Pushed < 3 || st[0].Target.SecretKey != "***" || st[0].LastSync.IsZero() {
		t.Fatalf("status: %+v", st)
	}
	// Versioned bucket: delete marker maps to DeleteObject.
	n.Must(200, a, "PUT", "/src?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	n.Must(200, a, "PUT", "/src/v.txt", []byte("v1"), nil)
	n.Must(204, a, "DELETE", "/src/v.txt", nil, nil)
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	if fp.puts["v.txt"] != "v1||" || fp.dels[len(fp.dels)-1] != "v.txt" {
		t.Fatalf("versioned: %+v %v", fp.puts, fp.dels)
	}
	fp.mu.Unlock()
	// Failure keeps the cursor and reports the error; disabling pauses.
	fp.fail = true
	n.Must(200, a, "PUT", "/src/e.txt", []byte("e"), nil)
	if err := w.Pass(ctx); err == nil {
		t.Fatal("expected push failure")
	}
	st = w.Status(ctx)
	if st[0].Lag != 1 || st[0].LastError == "" {
		t.Fatalf("lag after failure: %+v", st)
	}
	fp.fail = false
	rule.Targets[0].Enabled = false
	raw, _ = json.Marshal(rule)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "src", Value: string(raw)})
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if w.Status(ctx)[0].Lag != 1 {
		t.Fatal("disabled rule must not push")
	}
	rule.Targets[0].Enabled = true
	raw, _ = json.Marshal(rule)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "src", Value: string(raw)})
	w.Trigger()
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if w.Status(ctx)[0].Lag != 0 {
		t.Fatal("re-enabled rule must catch up")
	}
	if _, ok, _ := LoadRule(ctx, n.Core.DB, "nope"); ok {
		t.Fatal("no rule expected")
	}
	// Prefix filter on a second target; the first keeps everything.
	rule.Targets = append(rule.Targets, Target{Name: "second", Enabled: true, Prefix: "only/", Endpoint: "http://second:9000", Bucket: "m2", AccessKey: "a", SecretKey: "s"})
	raw, _ = json.Marshal(rule)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "src", Value: string(raw)})
	n.Must(200, a, "PUT", "/src/other.txt", []byte("x"), nil)
	n.Must(200, a, "PUT", "/src/only/in.txt", []byte("y"), nil)
	_ = w.Pass(ctx)
	fp.mu.Lock()
	// Both targets share the fake pusher: other.txt arrives once (first
	// target), only/in.txt twice (both targets).
	if fp.puts["other.txt"] != "x||" || fp.puts["only/in.txt"] != "y||" {
		t.Fatalf("multi-target: %+v", fp.puts)
	}
	fp.mu.Unlock()
	if st := w.Status(ctx); len(st) != 2 || st[0].Target.Name != "dest" || st[1].Target.Name != "second" || st[1].Lag != 0 {
		t.Fatalf("two targets: %+v", st)
	}

	// An idle target on another bucket (cursor 0, nothing pending) must not
	// pin the log of busy buckets; a paused target with pending entries must.
	n.Must(200, a, "PUT", "/idle", nil, nil)
	idle := Rule{Targets: []Target{{Name: "far", Enabled: true, Endpoint: "http://far:9000", Bucket: "m", AccessKey: "a", SecretKey: "s"}}}
	raw, _ = json.Marshal(idle)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "idle", Value: string(raw)})
	n.Must(200, a, "PUT", "/src/more.txt", []byte("z"), nil)
	_ = w.Pass(ctx)
	if cnt, _, _, _ := n.Core.DB.ReplicationLogStats(ctx); cnt != 0 {
		t.Fatalf("idle target pinned the log: %d entries", cnt)
	}
	rule.Targets[1].Enabled = false
	raw, _ = json.Marshal(rule)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "src", Value: string(raw)})
	n.Must(200, a, "PUT", "/src/only/held.txt", []byte("h"), nil)
	_ = w.Pass(ctx)
	if cnt, _, _, _ := n.Core.DB.ReplicationLogStats(ctx); cnt != 1 {
		t.Fatalf("paused target with pending entry should keep exactly it, log has %d", cnt)
	}
	// Removing the paused target releases the entry and its status.
	rule.Targets = rule.Targets[:1]
	raw, _ = json.Marshal(rule)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "src", Value: string(raw)})
	_ = w.Pass(ctx)
	if cnt, _, _, _ := n.Core.DB.ReplicationLogStats(ctx); cnt != 0 {
		t.Fatalf("log after removing target: %d", cnt)
	}
	w.mu.Lock()
	_, stale := w.status["src/second"]
	w.mu.Unlock()
	if stale {
		t.Fatal("status for removed target should be forgotten")
	}
}

func TestGlobalTargets(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	ctx := context.Background()
	fp := &fakePusher{puts: map[string]string{}}
	var seen []Target
	var seenMu sync.Mutex
	w := New(Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Log: slog.Default(),
		NewClient: func(t Target) Pusher {
			seenMu.Lock()
			seen = append(seen, t)
			seenMu.Unlock()
			return fp
		}}, time.Hour)
	n.Must(200, a, "PUT", "/one", nil, nil)
	n.Must(200, a, "PUT", "/two", nil, nil)
	n.Must(200, a, "PUT", "/two?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	n.Must(200, a, "PUT", "/one/a.txt", []byte("1"), nil)
	n.Must(200, a, "PUT", "/two/b.txt", []byte("2"), nil)

	global := Rule{Targets: []Target{{Name: "standby", Enabled: true, Endpoint: "http://standby:9000", AccessKey: "a", SecretKey: "s"}}}
	if err := global.ValidateGlobal(); err != nil {
		t.Fatal(err)
	}
	if global.Validate() == nil {
		t.Fatal("a server-wide target has no destination bucket and must fail bucket-level validation")
	}
	raw, _ := json.Marshal(global)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: GlobalKey, Value: string(raw)})
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	if fp.puts["a.txt"] != "1||" || fp.puts["b.txt"] != "2||" || fp.ensured != 2 || fp.ensuredVersioned != 1 {
		t.Fatalf("global push: puts=%v ensured=%d versioned=%d", fp.puts, fp.ensured, fp.ensuredVersioned)
	}
	fp.mu.Unlock()
	seenMu.Lock()
	for _, tg := range seen {
		if tg.Bucket != "one" && tg.Bucket != "two" {
			t.Fatalf("inherited target must mirror the source bucket name, got %q", tg.Bucket)
		}
		if !tg.Inherited {
			t.Fatal("inherited flag missing")
		}
	}
	seenMu.Unlock()
	st := w.Status(ctx)
	if len(st) != 2 || !st[0].Target.Inherited || st[0].Target.SecretKey != "***" || st[0].Lag != 0 || st[1].Lag != 0 {
		t.Fatalf("status: %+v", st)
	}
	if cnt, _, _, _ := n.Core.DB.ReplicationLogStats(ctx); cnt != 0 {
		t.Fatalf("log should be trimmed, has %d", cnt)
	}

	// A bucket-level target with the same name shadows the server-wide one.
	own := Rule{Targets: []Target{{Name: "standby", Enabled: false, Endpoint: "http://other:9000", Bucket: "elsewhere", AccessKey: "a", SecretKey: "s"}}}
	raw, _ = json.Marshal(own)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "one", Value: string(raw)})
	rules, err := w.rules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules["one"].Targets) != 1 || rules["one"].Targets[0].Inherited || rules["one"].Targets[0].Bucket != "elsewhere" {
		t.Fatalf("shadowing: %+v", rules["one"])
	}
	if len(rules["two"].Targets) != 1 || !rules["two"].Targets[0].Inherited {
		t.Fatalf("two: %+v", rules["two"])
	}
	// Ensure happens once per target per process.
	n.Must(200, a, "PUT", "/two/c.txt", []byte("3"), nil)
	_ = w.Pass(ctx)
	fp.mu.Lock()
	if fp.ensured != 2 || fp.puts["c.txt"] != "3||" {
		t.Fatalf("ensure cached: ensured=%d puts=%v", fp.ensured, fp.puts)
	}
	fp.mu.Unlock()
}

// TestRealDestination replicates from one test node to another over the
// real S3 API with the signing client.
func TestRealDestination(t *testing.T) {
	src := testutil.Start(t)
	dst := testutil.Start(t)
	ctx := context.Background()
	dst.Must(200, &dst.Admin, "PUT", "/mirror", nil, nil)
	dst.Must(200, &dst.Admin, "PUT", "/mirror?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	target := Target{Endpoint: dst.URL(), Bucket: "mirror", AccessKey: dst.Admin.AccessKeyID, SecretKey: dst.Admin.SecretKey, PathStyle: true}
	c := newClient(target, "")
	if created, err := c.EnsureBucket(ctx, s3client.BucketOptions{}); err != nil || created {
		t.Fatalf("existing bucket: created=%v err=%v", created, err)
	}
	bad := newClient(Target{Endpoint: dst.URL(), Bucket: "nope", AccessKey: "x", SecretKey: "y", PathStyle: true}, "")
	if _, err := bad.EnsureBucket(ctx, s3client.BucketOptions{}); err == nil {
		t.Fatal("bad credentials must fail")
	}
	// A missing destination bucket is created, versioned when asked.
	auto := newClient(Target{Endpoint: dst.URL(), Bucket: "auto-made", AccessKey: dst.Admin.AccessKeyID, SecretKey: dst.Admin.SecretKey, PathStyle: true}, "")
	if created, err := auto.EnsureBucket(ctx, s3client.BucketOptions{Versioned: true}); err != nil || !created {
		t.Fatalf("auto-create: created=%v err=%v", created, err)
	}
	resp0, body0 := dst.Call(&dst.Admin, "GET", "/auto-made?versioning", nil, nil)
	dst.Expect(200, resp0, body0)
	if !strings.Contains(string(body0), "Enabled") {
		t.Fatalf("auto-created bucket should be versioned: %s", body0)
	}
	if _, err := newClient(Target{Endpoint: "::bad"}, "").ObjectURL("k"); err == nil {
		t.Fatal("bad endpoint")
	}
	u, _ := newClient(Target{Endpoint: "https://s3.example.com", Bucket: "b"}, "").ObjectURL("a b/c")
	if u != "https://b.s3.example.com/a%20b/c" {
		t.Fatalf("virtual-host url: %s", u)
	}

	src.Must(200, &src.Admin, "PUT", "/src", nil, nil)
	target.Name, target.Enabled = "dst", true
	rule := Rule{Targets: []Target{target}}
	raw, _ := json.Marshal(rule)
	if _, err := src.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "src", Value: string(raw)}); err != nil {
		t.Fatal(err)
	}
	w := New(Deps{DB: src.Core.DB, Propose: src.Core.ProposeRaw, Chunks: src.Core.Chunks, Log: slog.Default()}, time.Hour)
	big := strings.Repeat("0123456789", 100000) // 1 MB, several chunks
	src.Must(200, &src.Admin, "PUT", "/src/big.bin", []byte(big), map[string]string{"Content-Type": "application/octet-stream", "x-amz-meta-origin": "src"})
	src.Must(200, &src.Admin, "PUT", "/src/gone.txt", []byte("bye"), nil)
	src.Must(204, &src.Admin, "DELETE", "/src/gone.txt", nil, nil)
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	resp, body := dst.Call(&dst.Admin, "GET", "/mirror/big.bin", nil, nil)
	dst.Expect(200, resp, body)
	if string(body) != big || resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("X-Amz-Meta-Origin") != "src" {
		t.Fatalf("replicated object mismatch: %d bytes %v", len(body), resp.Header)
	}
	// gone.txt was created then deleted: the destination ends up with no current version.
	resp, body = dst.Call(&dst.Admin, "GET", "/mirror/gone.txt", nil, nil)
	dst.Expect(404, resp, body)
	if st := w.Status(ctx); len(st) != 1 || st[0].Lag != 0 || st[0].LastError != "" {
		t.Fatalf("status: %+v", st)
	}
	_ = auth.Credentials{}
}

// TestBucketConfigAndOrigin covers lock-aware bucket creation, bucket
// configuration replication, loop prevention and config sync with a fake
// destination.
func TestBucketConfigAndOrigin(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	ctx := context.Background()
	fp := &fakePusher{puts: map[string]string{}}
	w := New(Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Log: slog.Default(),
		ServerID: func() string { return "s-me" }, NewClient: func(Target) Pusher { return fp }}, time.Hour)

	// A locked bucket replicates with lock + versioning and its policy.
	n.Must(200, a, "PUT", "/locked", nil, map[string]string{"x-amz-bucket-object-lock-enabled": "true"})
	n.Must(204, a, "PUT", "/locked?policy", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::locked/*"}]}`), nil)
	n.Must(200, a, "PUT", "/locked/a.txt", []byte("a"), nil)
	global := Rule{Targets: []Target{{Name: "standby", Enabled: true, SyncConfig: true, Endpoint: "http://standby:9000", AccessKey: "a", SecretKey: "s"}}}
	raw, _ := json.Marshal(global)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: GlobalKey, Value: string(raw)})
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	if fp.ensuredLocked != 1 || fp.puts["a.txt"] != "a||" || !strings.Contains(fp.config["policy"], "s3:GetObject") {
		t.Fatalf("locked bucket: locked=%d puts=%v config=%v", fp.ensuredLocked, fp.puts, fp.config)
	}
	fp.mu.Unlock()

	// Later configuration changes follow: policy removal, lifecycle, versioning.
	n.Must(204, a, "DELETE", "/locked?policy", nil, nil)
	n.Must(200, a, "PUT", "/locked?lifecycle", []byte(`<LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status><Filter></Filter><Expiration><Days>3</Days></Expiration></Rule></LifecycleConfiguration>`), nil)
	n.Must(200, a, "PUT", "/plain", nil, nil)
	n.Must(200, a, "PUT", "/plain?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	if fp.config["policy"] != "<deleted>" || !strings.Contains(fp.config["lifecycle"], "<Days>3</Days>") || !strings.Contains(fp.config["versioning"], "Enabled") {
		t.Fatalf("config changes: %v", fp.config)
	}
	fp.mu.Unlock()

	// A change that arrived from another server is applied but not forwarded.
	n.Must(200, a, "PUT", "/plain/from-peer.txt", []byte("p"), map[string]string{"X-Hogshead-Origin": "s-peer"})
	n.Must(200, a, "PUT", "/plain/local.txt", []byte("l"), nil)
	entries, _ := n.Core.DB.ReplicationLog(ctx, "plain", 0, 100)
	var sawOrigin bool
	for _, e := range entries {
		if e.Key == "from-peer.txt" && e.Origin == "s-peer" {
			sawOrigin = true
		}
	}
	if !sawOrigin {
		t.Fatalf("origin not recorded: %+v", entries)
	}
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	_, forwarded := fp.puts["from-peer.txt"]
	if forwarded || fp.puts["local.txt"] != "l||" {
		t.Fatalf("origin: %v", fp.puts)
	}
	fp.mu.Unlock()

	// Users, keys and settings are mirrored through the admin API; deletes too.
	creds := n.CreateUser("bob", meta.RoleReadWrite, []string{"plain"})
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: "uploads.max_age", Value: "48h"})
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: "replication.cursor:x:y", Value: "1"}) // never mirrored
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	joined := strings.Join(fp.admin, "\n")
	fp.mu.Unlock()
	if !strings.Contains(joined, "PUT /users/bob {") || !strings.Contains(joined, `"role":"read-write"`) ||
		!strings.Contains(joined, "PUT /users/bob/keys/"+creds.AccessKeyID+" {") || !strings.Contains(joined, `"secret":"`+creds.SecretKey+`"`) ||
		!strings.Contains(joined, "PUT /settings/uploads.max_age {\"value\":\"48h\"}") || strings.Contains(joined, "replication.cursor") {
		t.Fatalf("config sync calls:\n%s", joined)
	}
	u, _ := n.Core.DB.GetUserByName(ctx, "bob")
	_, _ = n.Core.Propose(ctx, &meta.DeleteUser{ID: u.ID})
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	joined = strings.Join(fp.admin, "\n")
	fp.mu.Unlock()
	if !strings.Contains(joined, "DELETE /keys/"+creds.AccessKeyID) || !strings.Contains(joined, "DELETE /users/bob") {
		t.Fatalf("deletes not mirrored:\n%s", joined)
	}
	// Status lists the config pseudo-bucket for the syncing target.
	var cfgRows int
	for _, st := range w.Status(ctx) {
		if st.Bucket == ConfigBucket {
			cfgRows++
		}
	}
	if cfgRows != 1 {
		t.Fatalf("config status rows: %d", cfgRows)
	}
}

// TestIdempotentTimestampedReplication checks that replicated writes reuse
// their source version id and time: duplicates are no-ops, a delayed older
// write does not become latest, and versioned purges reach the target.
func TestIdempotentTimestampedReplication(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	ctx := context.Background()
	n.Must(200, a, "PUT", "/vers", nil, nil)
	n.Must(200, a, "PUT", "/vers?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)

	rep := func(body, vid string, at int64) map[string]string {
		return map[string]string{"X-Hogshead-Origin": "s-peer", "X-Hogshead-Source-Version": vid, "X-Hogshead-Source-Time": strconv.FormatInt(at, 10)}
	}
	now := time.Now().UnixMilli()
	// Same replicated version twice: one row.
	n.Must(200, a, "PUT", "/vers/k", []byte("one"), rep("one", "VID-ONE", now-10_000))
	n.Must(200, a, "PUT", "/vers/k", []byte("one"), rep("one", "VID-ONE", now-10_000))
	resp, body := n.Call(a, "GET", "/vers?versions", nil, nil)
	n.Expect(200, resp, body)
	if c := strings.Count(string(body), "<Version>"); c != 1 {
		t.Fatalf("duplicate delivery created %d versions: %s", c, body)
	}
	// A newer local write, then a delayed older replicated write: latest stays the newer one.
	n.Must(200, a, "PUT", "/vers/k", []byte("newer"), nil)
	n.Must(200, a, "PUT", "/vers/k", []byte("stale"), rep("stale", "VID-STALE", now-5_000))
	resp, body = n.Call(a, "GET", "/vers/k", nil, nil)
	n.Expect(200, resp, body)
	if string(body) != "newer" {
		t.Fatalf("stale replicated write became latest: %q", body)
	}
	v, err := n.Core.DB.GetObject(ctx, "vers", "k", "VID-STALE")
	if err != nil || v.IsLatest || string(body) == "stale" {
		t.Fatalf("stale version should exist but not be latest: %+v %v", v, err)
	}
	// A delayed older delete marker does not hide the newer object either.
	n.Must(204, a, "DELETE", "/vers/k", nil, rep("", "VID-OLDMARKER", now-4_000))
	resp, body = n.Call(a, "GET", "/vers/k", nil, nil)
	n.Expect(200, resp, body)
	if string(body) != "newer" {
		t.Fatalf("stale marker hid the object: %d %q", resp.StatusCode, body)
	}
	// Deleting the newest version promotes by creation time: the delayed
	// marker (4 s old) is newer than the stale write (5 s old), so the key
	// reads as deleted; removing the marker then surfaces the stale write.
	cur, _ := n.Core.DB.GetObject(ctx, "vers", "k", "")
	n.Must(204, a, "DELETE", "/vers/k?versionId="+cur.VersionID, nil, nil)
	resp, _ = n.Call(a, "GET", "/vers/k", nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("marker should be latest after the newest version is removed, got %d", resp.StatusCode)
	}
	n.Must(204, a, "DELETE", "/vers/k?versionId=VID-OLDMARKER", nil, nil)
	resp, body = n.Call(a, "GET", "/vers/k", nil, nil)
	n.Expect(200, resp, body)
	if string(body) != "stale" {
		t.Fatalf("promotion by time: %q", body)
	}

	// Pushes carry the source headers, and a versioned purge is pushed as such.
	fp := &fakePusher{puts: map[string]string{}}
	w := New(Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Log: slog.Default(),
		ServerID: func() string { return "s-me" }, NewClient: func(Target) Pusher { return fp }}, time.Hour)
	rule := Rule{Targets: []Target{{Name: "dest", Enabled: true, Endpoint: "http://dest:9000", Bucket: "m", AccessKey: "a", SecretKey: "s"}}}
	raw, _ := json.Marshal(rule)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "vers", Value: string(raw)})
	_ = w.Pass(ctx) // drains and trims whatever is pending
	n.Must(200, a, "PUT", "/vers/p", []byte("p"), nil)
	pv, _ := n.Core.DB.GetObject(ctx, "vers", "p", "")
	n.Must(204, a, "DELETE", "/vers/p?versionId="+pv.VersionID, nil, nil)
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	// The put is skipped because that version was purged before the pass ran;
	// the purge itself travels as a versioned delete.
	if _, pushed := fp.puts["p"]; pushed || !strings.Contains(strings.Join(fp.dels, ","), "p@"+pv.VersionID) {
		t.Fatalf("versioned purge: puts=%v dels=%v", fp.puts, fp.dels)
	}
}

// TestObjectLockReplicates: per-object retention and legal hold travel with a
// replicated put, later changes to retention, hold and tags are replayed to
// the same version, and an expired retention is not forwarded.
func TestObjectLockReplicates(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	ctx := context.Background()
	fp := &fakePusher{puts: map[string]string{}}
	w := New(Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Log: slog.Default(),
		ServerID: func() string { return "s-me" }, NewClient: func(Target) Pusher { return fp }}, time.Hour)
	rule := Rule{Targets: []Target{{Name: "dest", Enabled: true, Endpoint: "http://dest:9000", Bucket: "m", AccessKey: "a", SecretKey: "s"}}}
	raw, _ := json.Marshal(rule)
	_, _ = n.Core.Propose(ctx, &meta.SetSetting{Key: RulePrefix + "vault", Value: string(raw)})
	n.Must(200, a, "PUT", "/vault", nil, map[string]string{"x-amz-bucket-object-lock-enabled": "true"})
	until := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	n.Must(200, a, "PUT", "/vault/doc", []byte("d"), map[string]string{"x-amz-object-lock-mode": "COMPLIANCE", "x-amz-object-lock-retain-until-date": until, "x-amz-object-lock-legal-hold": "ON"})
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	h := fp.hdrs["doc"]
	fp.mu.Unlock()
	if h["x-amz-object-lock-mode"] != "COMPLIANCE" || h["x-amz-object-lock-retain-until-date"] != until || h["x-amz-object-lock-legal-hold"] != "ON" {
		t.Fatalf("lock headers not forwarded: %v", h)
	}
	// Changes after upload: release the hold, set tags, extend retention.
	v, _ := n.Core.DB.GetObject(ctx, "vault", "doc", "")
	n.Must(200, a, "PUT", "/vault/doc?legal-hold&versionId="+v.VersionID, []byte("<LegalHold><Status>OFF</Status></LegalHold>"), nil)
	n.Must(200, a, "PUT", "/vault/doc?tagging&versionId="+v.VersionID, []byte("<Tagging><TagSet><Tag><Key>team</Key><Value>ops</Value></Tag></TagSet></Tagging>"), nil)
	later := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	n.Must(200, a, "PUT", "/vault/doc?retention&versionId="+v.VersionID, []byte("<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>"+later+"</RetainUntilDate></Retention>"), nil)
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	subs := strings.Join(fp.subs, "\n")
	fp.mu.Unlock()
	for _, want := range []string{
		"PUT doc@" + v.VersionID + "?retention <Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>" + later,
		"PUT doc@" + v.VersionID + "?legal-hold <LegalHold><Status>OFF</Status></LegalHold>",
		"PUT doc@" + v.VersionID + "?tagging <Tagging><TagSet><Tag><Key>team</Key><Value>ops</Value></Tag></TagSet></Tagging>",
	} {
		if !strings.Contains(subs, want) {
			t.Fatalf("missing replayed change %q in:\n%s", want, subs)
		}
	}
	// An expired retention is not forwarded on a put (the receiver would refuse it).
	w.d.Now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	n.Must(200, a, "PUT", "/vault/old", []byte("o"), map[string]string{"x-amz-object-lock-mode": "GOVERNANCE", "x-amz-object-lock-retain-until-date": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	h = fp.hdrs["old"]
	fp.mu.Unlock()
	if _, has := h["x-amz-object-lock-mode"]; has {
		t.Fatalf("expired retention forwarded: %v", h)
	}
}
