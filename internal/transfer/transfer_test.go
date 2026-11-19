package transfer

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/s3client"
	"github.com/enetforge/hogshead/internal/testutil"
)

func manager(t *testing.T, n *testutil.Node) *Manager {
	t.Helper()
	return New(context.Background(), Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Chunks: n.Core.Chunks, Cfg: n.Core.Cfg, Log: slog.Default()})
}

func wait(t *testing.T, m *Manager, id string) *Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		j, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.terminal() {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func TestExportImportRoundTrip(t *testing.T) {
	src := testutil.Start(t)
	dst := testutil.Start(t)
	ctx := context.Background()
	remote := s3client.Endpoint{Endpoint: dst.URL(), Bucket: "mirror", AccessKey: dst.Admin.AccessKeyID, SecretKey: dst.Admin.SecretKey, PathStyle: true}

	src.Must(200, &src.Admin, "PUT", "/data", nil, nil)
	src.Must(200, &src.Admin, "PUT", "/data/a.txt", []byte("alpha"), map[string]string{"Content-Type": "text/plain", "x-amz-meta-k": "v", "Cache-Control": "max-age=60"})
	big := strings.Repeat("0123456789", 100000) // 1 MB, several chunks
	src.Must(200, &src.Admin, "PUT", "/data/sub/big.bin", []byte(big), map[string]string{"Content-Type": "application/octet-stream"})
	src.Must(200, &src.Admin, "PUT", "/data/other/c.txt", []byte("c"), nil)
	src.Must(200, &src.Admin, "PUT", "/data/folder/", nil, nil)

	m := manager(t, src)
	// Validation.
	if _, err := m.Start(ctx, Spec{Direction: "sideways", Bucket: "data", Remote: remote}, "t"); err == nil {
		t.Fatal("bad direction accepted")
	}
	if _, err := m.Start(ctx, Spec{Direction: Export, Bucket: "nope", Remote: remote}, "t"); err == nil {
		t.Fatal("missing bucket accepted")
	}
	if _, err := m.Start(ctx, Spec{Direction: Export, Bucket: "data", Remote: s3client.Endpoint{Endpoint: "ftp://x", Bucket: "b", AccessKey: "a", SecretKey: "s"}}, "t"); err == nil {
		t.Fatal("bad endpoint accepted")
	}

	// Dry run copies nothing but counts everything.
	j, err := m.Start(ctx, Spec{Direction: Export, Bucket: "data", Remote: remote, DryRun: true}, "t")
	if err != nil {
		t.Fatal(err)
	}
	j = wait(t, m, j.ID)
	if j.State != StateDone || j.Copied != 4 || j.Listed != 4 || j.Spec.Remote.SecretKey != "" {
		t.Fatalf("dry run: %+v", j)
	}
	if r, _ := dst.Call(&dst.Admin, "GET", "/mirror/a.txt", nil, nil); r.StatusCode != 404 {
		t.Fatal("dry run wrote to the remote")
	}

	// Real export creates the remote bucket and copies metadata.
	j, err = m.Start(ctx, Spec{Direction: Export, Bucket: "data", Remote: remote, Concurrency: 2}, "t")
	if err != nil {
		t.Fatal(err)
	}
	j = wait(t, m, j.ID)
	if j.State != StateDone || j.Copied != 4 || j.Failed != 0 || j.Bytes != int64(len(big)+5+1) {
		t.Fatalf("export: %+v", j)
	}
	resp, body := dst.Call(&dst.Admin, "GET", "/mirror/a.txt", nil, nil)
	dst.Expect(200, resp, body)
	if string(body) != "alpha" || resp.Header.Get("X-Amz-Meta-K") != "v" || resp.Header.Get("Content-Type") != "text/plain" || resp.Header.Get("Cache-Control") != "max-age=60" {
		t.Fatalf("exported object: %q %v", body, resp.Header)
	}
	resp, body = dst.Call(&dst.Admin, "GET", "/mirror/sub/big.bin", nil, nil)
	dst.Expect(200, resp, body)
	if string(body) != big {
		t.Fatalf("big object mismatch: %d bytes", len(body))
	}

	// Second run skips everything unchanged; "never" also skips; "always" copies again.
	j, _ = m.Start(ctx, Spec{Direction: Export, Bucket: "data", Remote: remote}, "t")
	if j = wait(t, m, j.ID); j.State != StateDone || j.Copied != 0 || j.Skipped != 4 {
		t.Fatalf("rerun: %+v", j)
	}
	src.Must(200, &src.Admin, "PUT", "/data/a.txt", []byte("alpha2"), map[string]string{"Content-Type": "text/plain", "x-amz-meta-k": "v"})
	j, _ = m.Start(ctx, Spec{Direction: Export, Bucket: "data", Remote: remote, Overwrite: OverwriteNever}, "t")
	if j = wait(t, m, j.ID); j.Copied != 0 || j.Skipped != 4 {
		t.Fatalf("never: %+v", j)
	}
	j, _ = m.Start(ctx, Spec{Direction: Export, Bucket: "data", Remote: remote}, "t")
	if j = wait(t, m, j.ID); j.Copied != 1 || j.Skipped != 3 {
		t.Fatalf("changed: %+v", j)
	}
	j, _ = m.Start(ctx, Spec{Direction: Export, Bucket: "data", Remote: remote, Overwrite: OverwriteAlways}, "t")
	if j = wait(t, m, j.ID); j.Copied != 4 {
		t.Fatalf("always: %+v", j)
	}

	// Prefix mapping: data/sub/ -> mirror/x/.
	j, _ = m.Start(ctx, Spec{Direction: Export, Bucket: "data", Prefix: "sub/", Remote: remote, RemotePrefix: "x/"}, "t")
	if j = wait(t, m, j.ID); j.State != StateDone || j.Copied != 1 {
		t.Fatalf("prefix export: %+v", j)
	}
	resp, body = dst.Call(&dst.Admin, "GET", "/mirror/x/big.bin", nil, nil)
	dst.Expect(200, resp, body)

	// Import into a fresh local bucket, mapping mirror/x/ -> restored/y/.
	src.Must(200, &src.Admin, "PUT", "/restored", nil, nil)
	j, err = m.Start(ctx, Spec{Direction: Import, Bucket: "restored", Prefix: "y/", Remote: remote, RemotePrefix: "x/"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if j = wait(t, m, j.ID); j.State != StateDone || j.Copied != 1 {
		t.Fatalf("prefix import: %+v", j)
	}
	j, _ = m.Start(ctx, Spec{Direction: Import, Bucket: "restored", Remote: remote}, "t")
	// a.txt, sub/big.bin, other/c.txt and x/big.bin; the folder placeholder is skipped.
	if j = wait(t, m, j.ID); j.State != StateDone || j.Copied != 4 || j.Failed != 0 {
		t.Fatalf("import: %+v", j)
	}
	resp, body = src.Call(&src.Admin, "GET", "/restored/a.txt", nil, nil)
	src.Expect(200, resp, body)
	if string(body) != "alpha2" || resp.Header.Get("X-Amz-Meta-K") != "v" || resp.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("imported object: %q %v", body, resp.Header)
	}
	resp, body = src.Call(&src.Admin, "GET", "/restored/y/big.bin", nil, nil)
	src.Expect(200, resp, body)
	if string(body) != big {
		t.Fatal("imported big object mismatch")
	}
	v, err := src.Core.DB.GetObject(ctx, "restored", "a.txt", "")
	if err != nil || v.Owner == "" {
		t.Fatalf("imported owner: %+v %v", v, err)
	}
	j, _ = m.Start(ctx, Spec{Direction: Import, Bucket: "restored", Remote: remote}, "t")
	if j = wait(t, m, j.ID); j.Copied != 0 || j.Skipped != 4 {
		t.Fatalf("import rerun: %+v", j)
	}

	// Records: list newest first, delete a finished one, cancel refuses a finished one.
	jobs, err := m.List(ctx)
	if err != nil || len(jobs) < 5 || jobs[0].ID != j.ID {
		t.Fatalf("list: %d %v", len(jobs), err)
	}
	if err := m.Cancel(j.ID); err != ErrNotRunning {
		t.Fatalf("cancel finished: %v", err)
	}
	if err := m.Delete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, j.ID); err == nil {
		t.Fatal("deleted job still readable")
	}

	// Unreachable remote fails the job with a reason.
	bad := remote
	bad.Endpoint = "http://127.0.0.1:1"
	j, _ = m.Start(ctx, Spec{Direction: Import, Bucket: "restored", Remote: bad}, "t")
	if j = wait(t, m, j.ID); j.State != StateFailed || j.Error == "" {
		t.Fatalf("unreachable: %+v", j)
	}

	// A record left "running" by a dead process is marked interrupted on start.
	stale := Job{ID: "t-stale", State: StateRunning, CreatedAt: time.Now()}
	if err := m.persist(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	if err := manager(t, src).MarkInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(ctx, "t-stale"); got.State != StateInterrupted {
		t.Fatalf("interrupted: %+v", got)
	}
}

func TestCancel(t *testing.T) {
	src := testutil.Start(t)
	ctx := context.Background()
	src.Must(200, &src.Admin, "PUT", "/data", nil, nil)
	for i := 0; i < 30; i++ {
		src.Must(200, &src.Admin, "PUT", "/data/k"+string(rune('a'+i)), []byte("x"), nil)
	}
	m := manager(t, src)
	slow := &slowClient{}
	m.d.NewClient = func(s3client.Endpoint) Client { return slow }
	j, err := m.Start(ctx, Spec{Direction: Export, Bucket: "data", Remote: s3client.Endpoint{Endpoint: "http://slow", Bucket: "b", AccessKey: "a", SecretKey: "s"}, Concurrency: 1, Overwrite: OverwriteAlways}, "t")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	j = wait(t, m, j.ID)
	if j.State != StateCancelled || j.Copied >= 30 || j.Failed != 0 {
		t.Fatalf("cancel: %+v", j)
	}
	if _, err := meta.Decode(nil); err == nil {
		t.Fatal("sanity")
	}
}

// slowClient accepts everything slowly so a job can be cancelled mid-way.
type slowClient struct{}

func (*slowClient) EnsureBucket(context.Context, s3client.BucketOptions) (bool, error) {
	return false, nil
}
func (*slowClient) HeadObject(context.Context, string) (*s3client.ObjectInfo, error) {
	return nil, s3client.ErrNotFound
}
func (*slowClient) GetObject(context.Context, string) (*s3client.Object, error) {
	return nil, s3client.ErrNotFound
}
func (*slowClient) PutObject(ctx context.Context, _ string, body io.Reader, _ int64, _ map[string]string) error {
	_, _ = io.Copy(io.Discard, body)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	return nil
}
func (*slowClient) ListObjects(context.Context, string, string) (*s3client.Listing, error) {
	return &s3client.Listing{}, nil
}
