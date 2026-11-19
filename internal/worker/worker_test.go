package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/store"
)

type env struct {
	t   *testing.T
	db  *meta.DB
	fs  *store.FS
	now time.Time
	r   *Runner
}

func newEnv(t *testing.T, o Options) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := meta.Open(ctx, filepath.Join(dir, "meta.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fs, _ := store.NewFS(filepath.Join(dir, "chunks"), nil)
	e := &env{t: t, db: db, fs: fs, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e.r = New(Deps{
		DB: db, Chunks: fs, Local: fs,
		Propose: func(_ context.Context, c meta.Command) (meta.Result, error) { return db.ApplyCommand(0, c), nil },
		Now:     func() time.Time { return e.now },
	}, o)
	return e
}

func (e *env) apply(c meta.Command) {
	e.t.Helper()
	if r := e.db.ApplyCommand(0, c); r.Err != "" {
		e.t.Fatalf("%T: %s %s", c, r.Err, r.Msg)
	}
}

func (e *env) put(bucket, key string, data []byte, created time.Time, tags string) string {
	e.t.Helper()
	res, err := store.Write(context.Background(), e.fs, bytesReader(data), store.WriteOpts{})
	if err != nil {
		e.t.Fatal(err)
	}
	m := map[string]string{}
	if tags != "" {
		m["x-amz-tagging"] = tags
	}
	vid := fmt.Sprintf("v%d", created.UnixNano())
	e.apply(&meta.PutObjectVersion{Bucket: bucket, Key: key, VersionID: vid, Size: res.Size, ETag: res.MD5, Chunks: res.Chunks, Metadata: m, Now: created.UnixMilli()})
	return vid
}

func TestGCAndOrphans(t *testing.T) {
	e := newEnv(t, Options{GCGrace: time.Hour})
	ctx := context.Background()
	e.apply(&meta.CreateBucket{Name: "b", Owner: "u", Now: e.now.UnixMilli()})
	e.put("b", "k", []byte("data-one"), e.now, "")
	// Overwrite: old chunk becomes unreferenced at e.now.
	e.now = e.now.Add(time.Minute)
	e.put("b", "k", []byte("data-two"), e.now, "")
	sum := sha256.Sum256([]byte("data-one"))
	old := hex.EncodeToString(sum[:])
	if ok, _ := e.fs.Has(ctx, old); !ok {
		t.Fatal("old chunk should still exist")
	}
	// Within grace: nothing happens.
	if err := e.r.RunGC(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.fs.Has(ctx, old); !ok {
		t.Fatal("gc must respect grace")
	}
	e.now = e.now.Add(2 * time.Hour)
	// The file was touched by the overwrite's dedup hit; GC leaves recently
	// touched files alone until their mtime is older than the grace period.
	if err := e.r.RunGC(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.fs.Has(ctx, old); !ok {
		t.Fatal("recently touched chunk must survive the mtime guard")
	}
	oldPath := filepath.Join(e.fs.Root(), old[0:2], old[2:4], old)
	past := e.now.Add(-3 * time.Hour)
	_ = os.Chtimes(oldPath, past, past)
	if err := e.r.RunGC(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.fs.Has(ctx, old); ok {
		t.Fatal("old chunk should be gone")
	}
	if known, _ := e.db.ChunkKnown(ctx, old); known {
		t.Fatal("row should be released")
	}
	st := e.r.Stats()
	if st.ChunksReleased != 1 || st.BytesReclaimed != 8 || st.LastGC.IsZero() {
		t.Fatalf("stats %+v", st)
	}
	// Orphan on disk, never referenced.
	orphan := []byte("never committed")
	osum := sha256.Sum256(orphan)
	oh := hex.EncodeToString(osum[:])
	_ = e.fs.Put(ctx, oh, orphan)
	p := filepath.Join(e.fs.Root(), oh[0:2], oh[2:4], oh)
	if err := e.r.RunOrphanScan(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.fs.Has(ctx, oh); !ok {
		t.Fatal("young orphan must survive")
	}
	ancient := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(p, ancient, ancient)
	e.now = time.Now()
	if err := e.r.RunOrphanScan(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.fs.Has(ctx, oh); ok {
		t.Fatal("old orphan should be removed")
	}
	// Referenced chunks are never touched by the orphan scan.
	live := sha256.Sum256([]byte("data-two"))
	lh := hex.EncodeToString(live[:])
	lp := filepath.Join(e.fs.Root(), lh[0:2], lh[2:4], lh)
	_ = os.Chtimes(lp, ancient, ancient)
	_ = e.r.RunOrphanScan(ctx)
	if ok, _ := e.fs.Has(ctx, lh); !ok {
		t.Fatal("live chunk removed")
	}
}

func TestUploadCleanup(t *testing.T) {
	e := newEnv(t, Options{UploadMaxAge: 24 * time.Hour})
	ctx := context.Background()
	e.apply(&meta.CreateBucket{Name: "b", Owner: "u", Now: e.now.UnixMilli()})
	e.apply(&meta.CreateUpload{UploadID: "old", Bucket: "b", Key: "k", Now: e.now.Add(-48 * time.Hour).UnixMilli()})
	e.apply(&meta.CreateUpload{UploadID: "new", Bucket: "b", Key: "k2", Now: e.now.UnixMilli()})
	if err := e.r.RunUploadCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.GetUpload(ctx, "old"); err == nil {
		t.Fatal("old upload should be aborted")
	}
	if _, err := e.db.GetUpload(ctx, "new"); err != nil {
		t.Fatal("new upload must remain")
	}
	if e.r.Stats().UploadsAborted != 1 {
		t.Fatal("stats")
	}
	e2 := newEnv(t, Options{})
	if err := e2.r.RunUploadCleanup(ctx); err != nil {
		t.Fatal("disabled cleanup must be a no-op")
	}
}

const lifecycleXML = `<LifecycleConfiguration>
  <Rule><ID>tmp</ID><Status>Enabled</Status><Filter><Prefix>tmp/</Prefix></Filter><Expiration><Days>7</Days></Expiration></Rule>
  <Rule><ID>tagged</ID><Status>Enabled</Status><Filter><And><Prefix></Prefix><Tag><Key>ttl</Key><Value>short</Value></Tag></And></Filter><Expiration><Days>1</Days></Expiration></Rule>
  <Rule><ID>big</ID><Status>Enabled</Status><Filter><ObjectSizeGreaterThan>100</ObjectSizeGreaterThan></Filter><Expiration><Date>2026-02-01T00:00:00.000Z</Date></Expiration></Rule>
  <Rule><ID>off</ID><Status>Disabled</Status><Expiration><Days>1</Days></Expiration></Rule>
  <Rule><ID>noncur</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><NoncurrentVersionExpiration><NoncurrentDays>3</NoncurrentDays><NewerNoncurrentVersions>1</NewerNoncurrentVersions></NoncurrentVersionExpiration></Rule>
  <Rule><ID>markers</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>
  <Rule><ID>abort</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><AbortIncompleteMultipartUpload><DaysAfterInitiation>2</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>
</LifecycleConfiguration>`

func TestLifecycleUnversioned(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := context.Background()
	e.apply(&meta.CreateBucket{Name: "b", Owner: "u", Now: e.now.UnixMilli()})
	e.apply(&meta.SetBucketLifecycle{Name: "b", Lifecycle: lifecycleXML})
	start := e.now
	e.put("b", "tmp/old", []byte("x"), start.Add(-8*24*time.Hour), "")
	e.put("b", "tmp/fresh", []byte("x"), start.Add(-time.Hour), "")
	e.put("b", "keep", []byte("x"), start.Add(-100*24*time.Hour), "")
	e.put("b", "tagged", []byte("x"), start.Add(-2*24*time.Hour), "ttl=short")
	e.put("b", "tagged-other", []byte("x"), start.Add(-2*24*time.Hour), "ttl=long")
	e.put("b", "big", make([]byte, 200), start.Add(-time.Hour), "")
	e.put("b", "smallish", make([]byte, 50), start.Add(-time.Hour), "")
	e.apply(&meta.CreateUpload{UploadID: "stale", Bucket: "b", Key: "mp", Now: start.Add(-3 * 24 * time.Hour).UnixMilli()})
	e.apply(&meta.CreateUpload{UploadID: "recent", Bucket: "b", Key: "mp2", Now: start.Add(-time.Hour).UnixMilli()})
	if err := e.r.RunLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	lr, _ := e.db.ListObjects(ctx, "b", meta.ListOpts{})
	for _, o := range lr.Objects {
		remaining[o.Key] = true
	}
	want := map[string]bool{"tmp/fresh": true, "keep": true, "tagged-other": true, "big": true, "smallish": true}
	if fmt.Sprint(remaining) != fmt.Sprint(want) {
		t.Fatalf("remaining %v want %v", remaining, want)
	}
	if _, err := e.db.GetUpload(ctx, "stale"); err == nil {
		t.Fatal("stale upload should be aborted")
	}
	if _, err := e.db.GetUpload(ctx, "recent"); err != nil {
		t.Fatal("recent upload should remain")
	}
	// Date based rule fires once the date passes.
	e.now = time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	if err := e.r.RunLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	lr, _ = e.db.ListObjects(ctx, "b", meta.ListOpts{})
	for _, o := range lr.Objects {
		if o.Key == "big" {
			t.Fatal("big should have expired by date")
		}
	}
	st := e.r.Stats()
	if st.ObjectsExpired != 4 || st.UploadsAborted != 2 || st.LastLifecycle.IsZero() {
		t.Fatalf("stats %+v", st)
	}
	// Start/Stop round trip.
	ctx2, cancel := context.WithCancel(ctx)
	e.r.Start(ctx2)
	time.Sleep(20 * time.Millisecond)
	cancel()
	e.r.Stop()
}

func TestLifecycleVersioned(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := context.Background()
	e.apply(&meta.CreateBucket{Name: "b", Owner: "u", Now: e.now.UnixMilli()})
	e.apply(&meta.SetBucketVersioning{Name: "b", State: meta.VersioningEnabled})
	e.apply(&meta.SetBucketLifecycle{Name: "b", Lifecycle: lifecycleXML})
	start := e.now
	// Four versions of k: v1..v4, each a day apart, newest 1 day ago.
	var vids []string
	for i := 4; i >= 1; i-- {
		vids = append(vids, e.put("b", "k", []byte(fmt.Sprint("v", i)), start.Add(-time.Duration(i)*24*time.Hour), ""))
	}
	// vids[0]=oldest(v4, 4 days old) ... vids[3]=newest. Noncurrent: v3 (noncurrent since 3d), v2 (since 2d), v1... wait: newest is 1 day old.
	// Order newest first: n1 (1d), n2 (2d), n3 (3d), n4 (4d). n2 became noncurrent at n1's creation (1d ago), n3 at 2d ago, n4 at 3d ago.
	// NoncurrentDays=3, keep 1 newer: n2 is kept (newer noncurrent), n3 noncurrent for 2d -> keep, n4 noncurrent for 3d -> expire.
	// tmp/old expiration on a versioned bucket inserts a delete marker.
	e.put("b", "tmp/old", []byte("x"), start.Add(-10*24*time.Hour), "")
	// A key that only has a delete marker left.
	e.put("b", "gone", []byte("x"), start.Add(-20*24*time.Hour), "")
	e.apply(&meta.DeleteObject{Bucket: "b", Key: "gone", MarkerVersionID: "dm-gone", Now: start.Add(-10 * 24 * time.Hour).UnixMilli()})
	e.apply(&meta.DeleteObject{Bucket: "b", Key: "gone", VersionID: fmt.Sprintf("v%d", start.Add(-20*24*time.Hour).UnixNano()), Now: start.UnixMilli()})
	if err := e.r.RunLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	vl, _ := e.db.ListVersions(ctx, "b", meta.VersionListOpts{Prefix: "k"})
	got := map[string]bool{}
	for _, v := range vl.Versions {
		got[string([]byte(v.VersionID))] = true
	}
	if got[vids[0]] || !got[vids[1]] || !got[vids[2]] || !got[vids[3]] || len(vl.Versions) != 3 {
		t.Fatalf("noncurrent expiry: %v (oldest=%s)", got, vids[0])
	}
	tl, _ := e.db.ListVersions(ctx, "b", meta.VersionListOpts{Prefix: "tmp/"})
	if len(tl.Versions) != 2 || !tl.Versions[0].DeleteMarker {
		t.Fatalf("expired current version on versioned bucket should get a marker: %+v", tl.Versions)
	}
	gl, _ := e.db.ListVersions(ctx, "b", meta.VersionListOpts{Prefix: "gone"})
	if len(gl.Versions) != 0 {
		t.Fatalf("lone delete marker should be removed: %+v", gl.Versions)
	}
	st := e.r.Stats()
	if st.VersionsExpired != 1 || st.MarkersRemoved != 1 || st.ObjectsExpired != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestParseRulesBadInput(t *testing.T) {
	if parseRules("") != nil || parseRules("<nope") != nil {
		t.Fatal("bad input must yield no rules")
	}
	rules := parseRules(lifecycleXML)
	if len(rules) != 6 {
		t.Fatalf("expected 6 enabled rules, got %d", len(rules))
	}
}
