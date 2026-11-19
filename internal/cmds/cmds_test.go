package cmds

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/cli"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/testutil"
	"github.com/enetforge/hogshead/internal/transfer"
	"github.com/enetforge/hogshead/internal/usage"
	"github.com/enetforge/hogshead/internal/wire"
)

func init() { SetHandlerFactory(wire.Handlers) }

// run executes a CLI command against the admin server and captures stdout.
func run(t *testing.T, srv *httptest.Server, n *testutil.Node, args ...string) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	full := append(args, "--server", srv.URL, "--access-key", n.Admin.AccessKeyID, "--secret-key", n.Admin.SecretKey)
	var errOut bytes.Buffer
	err := cli.Execute(context.Background(), Root(), full, &errOut)
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out) + errOut.String(), err
}

func TestManagementCommands(t *testing.T) {
	n := testutil.Start(t)
	h := admin.New(admin.Deps{
		DB: n.Core.DB, Propose: n.Core.ProposeRaw, Cfg: n.Core.Cfg, Version: "t", Started: time.Now(),
		ConfigPath: "/cfg", Reload: func() error { return nil }, PendingRestart: func() []string { return nil },
		Transfers: transfer.New(context.Background(), transfer.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Cfg: n.Core.Cfg}),
		ServerID:  func() string { return "s-test" }, Name: func() string { return "hogshead-test" },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	must := func(args ...string) string {
		t.Helper()
		out, err := run(t, srv, n, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	fail := func(args ...string) {
		t.Helper()
		if _, err := run(t, srv, n, args...); err == nil {
			t.Fatalf("%v should fail", args)
		}
	}
	if out := must("status"); !strings.Contains(out, "uptime") || !strings.Contains(out, "hogshead-test") || !strings.Contains(out, "s-test") {
		t.Fatal(out)
	}
	if out := must("peers"); !strings.Contains(out, "no other servers") {
		t.Fatal(out)
	}
	if out := must("status", "--json"); !strings.Contains(out, `"version": "t"`) {
		t.Fatal(out)
	}
	must("user", "add", "bob", "--role", "read-only", "--buckets", "a,b")
	fail("user", "add")
	if out := must("user", "list"); !strings.Contains(out, "bob") || !strings.Contains(out, "a,b") {
		t.Fatal(out)
	}
	if out := must("user", "show", "bob"); !strings.Contains(out, "read-only") {
		t.Fatal(out)
	}
	fail("user", "show")
	fail("user", "passwd", "bob", "--password", "short")
	must("user", "passwd", "bob", "--password", "longenough1")
	if out := must("user", "show", "bob", "--json"); !strings.Contains(out, `"has_password": true`) {
		t.Fatal(out)
	}
	must("user", "passwd", "bob", "--clear")
	// Wholesale import creates or replaces a user and a key with a known secret.
	must("user", "import", "carol", "--role", "read-only", "--buckets", "a", "--password-hash", "$2a$10$x")
	if out := must("user", "show", "carol", "--json"); !strings.Contains(out, `"has_password": true`) || !strings.Contains(out, `"read-only"`) {
		t.Fatal(out)
	}
	must("user", "import", "carol", "--role", "admin")
	if out := must("user", "show", "carol", "--json"); !strings.Contains(out, `"has_password": false`) || !strings.Contains(out, `"admin"`) {
		t.Fatal(out)
	}
	must("key", "import", "carol", "AKIAIMPORTED0000001", "--secret", "topsecret", "--label", "synced")
	if out := must("key", "list", "carol"); !strings.Contains(out, "AKIAIMPORTED0000001") || !strings.Contains(out, "synced") {
		t.Fatal(out)
	}
	must("key", "import", "carol", "AKIAIMPORTED0000001", "--secret", "topsecret", "--label", "synced", "--disabled")
	if out := must("key", "list", "carol"); !strings.Contains(out, "true") {
		t.Fatal(out)
	}
	fail("key", "import", "carol", "AKIAIMPORTED0000001")
	if out := must("user", "show", "bob", "--json"); !strings.Contains(out, `"has_password": false`) {
		t.Fatal(out)
	}
	must("user", "set", "bob", "--role", "read-write", "--buckets", "*", "--disable")
	if out := must("user", "show", "bob", "--json"); !strings.Contains(out, `"disabled": true`) {
		t.Fatal(out)
	}
	must("user", "set", "bob", "--enable")
	fail("user", "set")
	out := must("key", "create", "bob", "--label", "l1", "--json")
	var ck admin.CreatedKey
	if err := json.Unmarshal([]byte(out), &ck); err != nil || ck.SecretAccessKey == "" {
		t.Fatalf("key create: %s", out)
	}
	must("key", "create", "bob")
	fail("key", "create")
	if out := must("key", "list", "--user", "bob"); !strings.Contains(out, ck.AccessKeyID) {
		t.Fatal(out)
	}
	must("key", "list")
	must("key", "disable", ck.AccessKeyID)
	must("key", "enable", ck.AccessKeyID)
	fail("key", "enable")
	must("key", "revoke", ck.AccessKeyID)
	fail("key", "revoke")
	fail("key", "revoke", ck.AccessKeyID)

	must("bucket", "create", "photos", "--versioning", "Enabled", "--owner", "bob")
	fail("bucket", "create")
	if out := must("bucket", "list"); !strings.Contains(out, "photos") {
		t.Fatal(out)
	}
	polFile := filepath.Join(t.TempDir(), "policy.json")
	_ = os.WriteFile(polFile, []byte(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}`), 0o600)
	must("bucket", "set", "photos", "--policy", "@"+polFile)
	if out := must("bucket", "show", "photos"); !strings.Contains(out, "Statement") {
		t.Fatal(out)
	}
	must("bucket", "set", "photos", "--policy", "none", "--public")
	must("bucket", "set", "photos", "--private", "--cors", "none", "--lifecycle", "none")
	fail("bucket", "set", "photos", "--policy", "@/nonexistent")
	fail("bucket", "set")
	fail("bucket", "show")
	must("bucket", "rm", "photos")
	fail("bucket", "rm", "photos")
	fail("bucket", "rm")
	// A versioned bucket that looks empty still holds a version and a marker.
	must("bucket", "create", "ghost", "--versioning", "Enabled")
	n.Must(200, &n.Admin, "PUT", "/ghost/doc.pdf", []byte("x"), nil)
	n.Must(204, &n.Admin, "DELETE", "/ghost/doc.pdf", nil, nil)
	if out := must("bucket", "show", "ghost", "--json"); !strings.Contains(out, `"objects": 0`) || !strings.Contains(out, `"versions": 2`) || !strings.Contains(out, `"delete_markers": 1`) {
		t.Fatal(out)
	}
	fail("bucket", "rm", "ghost")
	must("bucket", "rm", "ghost", "--force")
	fail("bucket", "show", "ghost")
	// Compliance retention blocks a forced purge.
	n.Must(200, &n.Admin, "PUT", "/vault", nil, map[string]string{"x-amz-bucket-object-lock-enabled": "true"})
	n.Must(200, &n.Admin, "PUT", "/vault/keep", []byte("k"), map[string]string{"x-amz-object-lock-mode": "COMPLIANCE", "x-amz-object-lock-retain-until-date": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	fail("bucket", "rm", "vault", "--force")

	must("bucket", "create", "repl")
	fail("bucket", "replicate", "repl", "--to", "ftp://x")
	must("bucket", "replicate", "repl", "--to", "http://dest:9000", "--bucket", "mirror", "--dest-key", "a", "--dest-secret", "s", "--path-style")
	must("bucket", "replicate", "repl", "--name", "offsite", "--to", "https://offsite.example", "--bucket", "mirror2", "--dest-key", "a", "--dest-secret", "s")
	if out := must("bucket", "replicate", "repl", "--status"); !strings.Contains(out, "dest:9000/mirror") || !strings.Contains(out, "offsite") {
		t.Fatal(out)
	}
	must("bucket", "replicate", "repl", "--name", "offsite", "--disable")
	if out := must("bucket", "replicate"); !strings.Contains(out, "false") || !strings.Contains(out, "true") {
		t.Fatal(out)
	}
	fail("bucket", "replicate", "repl", "--disable")
	must("bucket", "replicate", "repl", "--name", "offsite", "--remove")
	if out := must("bucket", "replicate", "repl", "--status"); strings.Contains(out, "offsite") {
		t.Fatal(out)
	}
	must("bucket", "replicate", "repl", "--disable") // single target: --name optional
	if out := must("bucket", "replicate", "repl", "--status"); !strings.Contains(out, "false") {
		t.Fatal(out)
	}
	fail("bucket", "replicate", "repl", "--name", "bad name", "--to", "http://x:1", "--bucket", "b", "--dest-key", "a", "--dest-secret", "s")
	must("bucket", "replicate", "repl", "--remove")
	fail("bucket", "replicate", "repl", "--status")

	// Transfers: a dry-run export against an unreachable remote still lists.
	fail("transfer", "export", "repl", "--to", "ftp://x", "--bucket", "b", "--dest-key", "a", "--dest-secret", "s")
	fail("transfer", "export", "nosuchbucket", "--to", "http://127.0.0.1:1", "--bucket", "b", "--dest-key", "a", "--dest-secret", "s")
	n.Must(200, &n.Admin, "PUT", "/repl/one.txt", []byte("1"), nil)
	out = must("transfer", "export", "repl", "--to", "http://127.0.0.1:1", "--bucket", "b", "--dest-key", "a", "--dest-secret", "s", "--dry-run", "--overwrite", "always", "--wait")
	if !strings.Contains(out, "done") || !strings.Contains(out, "dry run") {
		t.Fatal(out)
	}
	if out := must("transfer", "list"); !strings.Contains(out, "export") || !strings.Contains(out, "repl/") {
		t.Fatal(out)
	}
	id := strings.Fields(must("transfer", "list"))[11]
	if out := must("transfer", "show", id); !strings.Contains(out, id) {
		t.Fatal(out)
	}
	must("transfer", "cancel", id) // finished: removes the record
	fail("transfer", "show", id)
	fail("transfer", "import", "repl", "--from", "http://127.0.0.1:1", "--bucket", "b", "--dest-key", "a", "--dest-secret", "s", "--wait") // unreachable remote fails

	// Usage counters: traffic through the S3 API shows up once flushed.
	use := usage.New(n.Core.ProposeRaw, nil)
	n.API.SetAccess(use.Handle)
	n.Must(200, &n.Admin, "PUT", "/repl/u1.txt", []byte("1"), nil)
	n.Must(200, &n.Admin, "GET", "/repl/u1.txt", nil, nil)
	n.Must(404, &n.Admin, "GET", "/repl/missing.txt", nil, nil)
	if err := use.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := must("usage", "--by", "bucket,class", "--bucket", "repl"); !strings.Contains(out, "read") || !strings.Contains(out, "write") || !strings.Contains(out, "total: 3 requests, 1 errors") {
		t.Fatal(out)
	}
	if out := must("usage", "--by", "key", "--json"); !strings.Contains(out, n.Admin.AccessKeyID) {
		t.Fatal(out)
	}
	out = must("errors", "--recent", "5")
	if !strings.Contains(out, "NoSuchKey") || !strings.Contains(out, "does not exist") || !strings.Contains(out, "repl/missing.txt") {
		t.Fatal(out)
	}
	if out := must("errors", "--code", "AccessDenied"); !strings.Contains(out, "no failed requests") {
		t.Fatal(out)
	}
	fail("usage", "--by", "colour")
	fail("usage", "--since", "yesterday")

	// Internal and cellar settings are not writable through the generic route.
	fail("setting", "set", "server.id", "x")
	fail("setting", "set", "ui.session_secret", "x")
	fail("setting", "set", "cellar.id", "c-rogue") // not in a cellar
	fail("setting", "set", "cellar.members", "[]")

	// Server-wide targets.
	fail("replicate", "--to", "ftp://x", "--dest-key", "a", "--dest-secret", "s")
	must("replicate", "--to", "http://standby:9000", "--dest-key", "a", "--dest-secret", "s", "--path-style", "--sync-config")
	if out := must("replicate"); !strings.Contains(out, "standby:9000") || !strings.Contains(out, "true") {
		t.Fatal(out)
	}
	fail("bucket", "replicate", "repl", "--name", "x", "--to", "http://x:1", "--bucket", "b", "--dest-key", "a", "--dest-secret", "s", "--sync-config") // bucket-level targets cannot sync config
	must("replicate", "--name", "standby:9000", "--no-sync-config")
	if out := must("bucket", "replicate", "repl", "--status"); !strings.Contains(out, "server-wide") || !strings.Contains(out, "standby:9000/repl") {
		t.Fatal(out)
	}
	fail("bucket", "replicate", "repl", "--name", "standby:9000", "--remove") // inherited: 409
	must("replicate", "--name", "standby:9000", "--disable")
	if out := must("replicate"); !strings.Contains(out, "false") {
		t.Fatal(out)
	}
	must("replicate", "--name", "standby:9000", "--remove")
	fail("replicate", "--name", "standby:9000", "--remove")
	must("setting", "set", "gc.grace", "48h")
	must("setting", "set", "gc.grace")
	fail("setting", "set")
	if out := must("setting", "list"); !strings.Contains(out, "gc.grace") {
		t.Fatal(out)
	}
	if out := must("audit", "--limit", "50"); !strings.Contains(out, "user.create") {
		t.Fatal(out)
	}
	if out := must("config", "show"); !strings.Contains(out, "data_dir") {
		t.Fatal(out)
	}
	must("config", "reload")
	if out := must("api", "GET", "/me"); !strings.Contains(out, "admin") {
		t.Fatal(out)
	}
	must("api", "POST", "/users", `{"name":"viaapi"}`)
	fail("api", "GET")
	must("user", "rm", "bob")
	fail("user", "rm", "bob")
	fail("user", "rm")
	// Missing credentials for --server.
	var eb bytes.Buffer
	if err := cli.Execute(context.Background(), Root(), []string{"status", "--server", srv.URL}, &eb); err == nil {
		t.Fatal("server without keys must fail")
	}
	if err := cli.Execute(context.Background(), Root(), []string{"status", "--socket", "/nonexistent.sock"}, &eb); err == nil {
		t.Fatal("missing socket must fail")
	}
	if humanBytes(500) != "500 B" || humanBytes(1536) != "1.5 KiB" || humanBytes(3<<30) != "3.0 GiB" {
		t.Fatal("humanBytes")
	}
	if s, _ := readArgOrFile("plain"); s != "plain" {
		t.Fatal("readArgOrFile")
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestQuickstartAndServe(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	listen := freePort(t)
	var out bytes.Buffer
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := cli.Execute(context.Background(), Root(), []string{"quickstart", "--data-dir", filepath.Join(dir, "data"), "--config", cfgPath, "--listen", listen, "--encrypt", "--compress", "--systemd"}, &out)
	_ = w.Close()
	os.Stdout = old
	printed, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("%v %s", err, out.String())
	}
	if !strings.Contains(string(printed), "AWS_ACCESS_KEY_ID=") || !strings.Contains(string(printed), "master.key") || !strings.Contains(string(printed), "[Service]") {
		t.Fatalf("quickstart output: %s", printed)
	}
	if err := cli.Execute(context.Background(), Root(), []string{"quickstart", "--config", cfgPath}, &out); err == nil {
		t.Fatal("existing config must be refused")
	}
	// Serve with the socket disabled (temp paths are too long for unix sockets).
	t.Setenv("HOGSHEAD_ADMIN_SOCKET", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cli.Execute(ctx, Root(), []string{"serve", "--config", cfgPath}, &out) }()
	var resp *http.Response
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + listen + "/healthz") //nolint:noctx
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never came up: %v\n%s", err, out.String())
	}
	resp, _ = http.Get("http://" + listen + "/_ui/") //nolint:noctx
	if resp.StatusCode != 200 {
		t.Fatal("ui")
	}
	_ = resp.Body.Close()
	resp, _ = http.Get("http://" + listen + "/") //nolint:noctx
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("anonymous list buckets: %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := cli.Execute(context.Background(), Root(), []string{"serve", "--config", "/nonexistent"}, &out); err == nil {
		t.Fatal("bad config path")
	}
	if err := cli.Execute(context.Background(), Root(), []string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	_ = meta.RoleAdmin
}

func TestServeInitFirstStart(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "data", "config.toml")
	listen := freePort(t)
	t.Setenv("HOGSHEAD_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("HOGSHEAD_LISTEN", listen)
	t.Setenv("HOGSHEAD_ADMIN_SOCKET", "")
	t.Setenv("HOGSHEAD_DISCOVERY_ENABLED", "false")
	t.Setenv("HOGSHEAD_INIT_ACCESS_KEY", "AKIDEXAMPLE")
	t.Setenv("HOGSHEAD_INIT_SECRET_KEY", "a-secret-key-of-enough-length")
	t.Setenv("HOGSHEAD_INIT_PASSWORD", "correct horse")

	login := func() int {
		body := strings.NewReader(`{"user":"admin","password":"correct horse"}`)
		resp, err := http.Post("http://"+listen+"/_ui/api/login", "application/json", body) //nolint:noctx
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// Two starts: the first creates everything, the second must reuse it.
	for start := 1; start <= 2; start++ {
		var out bytes.Buffer
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- cli.Execute(ctx, Root(), []string{"serve", "--init", "--config", cfgPath}, &out) }()
		status := 0
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && status == 0; {
			if status = login(); status == 0 {
				time.Sleep(100 * time.Millisecond)
			}
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("start %d: serve: %v %s", start, err, out.String())
		}
		if status != http.StatusOK {
			t.Fatalf("start %d: login with the preset password: status %d", start, status)
		}
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config file not written: %v", err)
	}
}

func TestSetupRejectsWeakPresetCredentials(t *testing.T) {
	cases := map[string]setupOptions{
		"secret without key": {SecretKey: "a-secret-key-of-enough-length"},
		"short secret":       {AccessKey: "AKIDEXAMPLE", SecretKey: "short"},
		"bad access key":     {AccessKey: "no spaces", SecretKey: "a-secret-key-of-enough-length"},
		"short password":     {Password: "short"},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			o.ConfigOut = filepath.Join(t.TempDir(), "config.toml")
			o.DataDir = t.TempDir()
			if _, err := setup(context.Background(), o); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A rotation signs every web UI session out, lets people log straight back
// in, and survives a restart: the new secret is the stored one.
func TestRotateSessionsSignsEveryoneOut(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "data", "config.toml")
	listen := freePort(t)
	t.Setenv("HOGSHEAD_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("HOGSHEAD_LISTEN", listen)
	t.Setenv("HOGSHEAD_ADMIN_SOCKET", "")
	t.Setenv("HOGSHEAD_DISCOVERY_ENABLED", "false")
	t.Setenv("HOGSHEAD_INIT_PASSWORD", "correct horse")
	base := "http://" + listen

	login := func() *http.Cookie {
		resp, err := http.Post(base+"/_ui/api/login", "application/json", strings.NewReader(`{"user":"admin","password":"correct horse"}`)) //nolint:noctx
		if err != nil {
			return nil
		}
		defer func() { _ = resp.Body.Close() }()
		for _, c := range resp.Cookies() {
			if c.Value != "" {
				return c
			}
		}
		return nil
	}
	call := func(method, path string, c *http.Cookie) int {
		req, _ := http.NewRequest(method, base+path, nil) //nolint:noctx
		req.AddCookie(c)
		req.Header.Set("X-Hogshead-Session", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	serve := func() (stop func()) {
		var out bytes.Buffer
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- cli.Execute(ctx, Root(), []string{"serve", "--init", "--config", cfgPath}, &out) }()
		return func() {
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("serve: %v %s", err, out.String())
			}
		}
	}
	waitLogin := func() *http.Cookie {
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if c := login(); c != nil {
				return c
			}
		}
		t.Fatal("login never succeeded")
		return nil
	}

	stop := serve()
	before := waitLogin()
	if s := call("GET", "/_ui/api/me", before); s != 200 {
		t.Fatalf("session before rotation: %d", s)
	}
	if s := call("POST", "/_admin/v1/sessions/rotate", before); s != 200 {
		t.Fatalf("rotate: %d", s)
	}
	if s := call("GET", "/_ui/api/me", before); s != 403 {
		t.Fatalf("old session after rotation: %d, want 403", s)
	}
	after := waitLogin()
	if s := call("GET", "/_ui/api/me", after); s != 200 {
		t.Fatalf("new session after rotation: %d", s)
	}
	stop()

	stop = serve()
	defer stop()
	waitLogin()
	if s := call("GET", "/_ui/api/me", after); s != 200 {
		t.Fatalf("session issued after the rotation must survive a restart: %d", s)
	}
	if s := call("GET", "/_ui/api/me", before); s != 403 {
		t.Fatalf("pre-rotation session after a restart: %d, want 403", s)
	}
}
