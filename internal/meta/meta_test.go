package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "meta.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustApply(t *testing.T, db *DB, cmd Command) Result {
	t.Helper()
	r := db.ApplyCommand(0, cmd)
	if r.Err != "" {
		t.Fatalf("%T: %s: %s", cmd, r.Err, r.Msg)
	}
	return r
}

func applyErr(t *testing.T, db *DB, cmd Command, want Code) {
	t.Helper()
	r := db.ApplyCommand(0, cmd)
	if r.Err != want {
		t.Fatalf("%T: want %s got %q (%s)", cmd, want, r.Err, r.Msg)
	}
}

func chunk(i int) ChunkRef { return ChunkRef{Hash: fmt.Sprintf("%064x", i), Size: 100} }

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cmd := &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "v1", Chunks: []ChunkRef{chunk(1)}, Now: 5}
	b, err := Encode(cmd)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.(*PutObjectVersion)
	if !ok || got.Key != "k" || got.Chunks[0] != chunk(1) {
		t.Fatalf("round trip: %+v", back)
	}
	if _, err := Decode([]byte(`{"n":"nope","p":{}}`)); err == nil {
		t.Fatal("unknown command must fail")
	}
}

func TestBucketLifecycle(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	mustApply(t, db, &CreateBucket{Name: "b1", Owner: "u1", Now: 1000})
	applyErr(t, db, &CreateBucket{Name: "b1", Owner: "u1", Now: 1000}, CodeBucketAlreadyOwned)
	applyErr(t, db, &CreateBucket{Name: "b1", Owner: "u2", Now: 1000}, CodeBucketAlreadyExists)
	b, err := db.GetBucket(ctx, "b1")
	if err != nil || b.Owner != "u1" || b.CreatedAt.UnixMilli() != 1000 {
		t.Fatalf("get bucket: %+v %v", b, err)
	}
	if _, err := db.GetBucket(ctx, "nope"); !errors.Is(err, ErrNoSuchBucket) {
		t.Fatalf("want NoSuchBucket got %v", err)
	}
	mustApply(t, db, &PutObjectVersion{Bucket: "b1", Key: "k", VersionID: "x", Chunks: []ChunkRef{chunk(1)}, Now: 1})
	applyErr(t, db, &DeleteBucket{Name: "b1"}, CodeBucketNotEmpty)
	mustApply(t, db, &DeleteObject{Bucket: "b1", Key: "k", Now: 2})
	mustApply(t, db, &SetBucketPolicy{Name: "b1", Policy: `{"Version":"2012-10-17"}`})
	mustApply(t, db, &SetBucketCORS{Name: "b1", CORS: "<CORS/>"})
	mustApply(t, db, &SetBucketLifecycle{Name: "b1", Lifecycle: "<LC/>"})
	b, _ = db.GetBucket(ctx, "b1")
	if b.Policy == "" || b.CORS == "" || b.Lifecycle == "" {
		t.Fatalf("bucket fields: %+v", b)
	}
	mustApply(t, db, &DeleteBucket{Name: "b1"})
	applyErr(t, db, &DeleteBucket{Name: "b1"}, CodeNoSuchBucket)
	list, _ := db.ListBuckets(ctx, "")
	if len(list) != 0 {
		t.Fatal("bucket should be gone")
	}
}

func TestUnversionedObjects(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	mustApply(t, db, &CreateBucket{Name: "b", Owner: "u", Now: 1})
	r := mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "ignored", Size: 100, ETag: "e1", Chunks: []ChunkRef{chunk(1), chunk(2)}, Now: 10, ContentType: "text/plain", Metadata: map[string]string{"x-amz-meta-a": "1"}})
	var pr PutObjectResult
	_ = r.Decode(&pr)
	if pr.VersionID != NullVersionID {
		t.Fatalf("unversioned must store null version, got %q", pr.VersionID)
	}
	o, err := db.GetObject(ctx, "b", "k", "")
	if err != nil || o.ETag != "e1" || len(o.Chunks) != 2 || o.ContentType != "text/plain" || o.Metadata["x-amz-meta-a"] != "1" {
		t.Fatalf("get: %+v %v", o, err)
	}
	// Overwrite releases old chunks.
	mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "x", Size: 50, ETag: "e2", Chunks: []ChunkRef{chunk(2), chunk(3)}, Now: 20})
	o, _ = db.GetObject(ctx, "b", "k", "")
	if o.ETag != "e2" {
		t.Fatal("overwrite failed")
	}
	un, _ := db.UnreferencedChunks(ctx, 1<<62, 10)
	if len(un) != 1 || un[0].Hash != chunk(1).Hash {
		t.Fatalf("chunk 1 should be unreferenced: %+v", un)
	}
	// Conditional writes.
	applyErr(t, db, &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "x", IfNoneMatchAny: true, Now: 21}, CodePreconditionFailed)
	applyErr(t, db, &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "x", IfMatchETag: "nope", Now: 21}, CodePreconditionFailed)
	mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "x", IfMatchETag: `"e2"`, ETag: "e3", Now: 22})
	mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "new", VersionID: "x", IfNoneMatchAny: true, ETag: "e4", Now: 22})

	r = mustApply(t, db, &DeleteObject{Bucket: "b", Key: "k", Now: 30})
	var dr DeleteObjectResult
	_ = r.Decode(&dr)
	if !dr.Existed || dr.DeleteMarker {
		t.Fatalf("delete result: %+v", dr)
	}
	if _, err := db.GetObject(ctx, "b", "k", ""); !errors.Is(err, ErrNoSuchKey) {
		t.Fatalf("want NoSuchKey got %v", err)
	}
	r = mustApply(t, db, &DeleteObject{Bucket: "b", Key: "k", Now: 31})
	dr = DeleteObjectResult{}
	_ = r.Decode(&dr)
	if dr.Existed {
		t.Fatal("second delete should report not existed")
	}
	un, _ = db.UnreferencedChunks(ctx, 1<<62, 10)
	if len(un) != 3 {
		t.Fatalf("all chunks unreferenced: %+v", un)
	}
	mustApply(t, db, &ReleaseChunks{Hashes: []string{chunk(1).Hash}})
	known, _ := db.ChunkKnown(ctx, chunk(1).Hash)
	if known {
		t.Fatal("released chunk should be gone")
	}
}

func TestVersionedObjects(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	mustApply(t, db, &CreateBucket{Name: "b", Owner: "u", Now: 1})
	applyErr(t, db, &SetBucketVersioning{Name: "b", State: "Maybe"}, CodeInvalidArgument)
	mustApply(t, db, &SetBucketVersioning{Name: "b", State: VersioningEnabled})
	mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "v1", ETag: "e1", Chunks: []ChunkRef{chunk(1)}, Now: 10})
	mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "v2", ETag: "e2", Chunks: []ChunkRef{chunk(2)}, Now: 20})
	o, _ := db.GetObject(ctx, "b", "k", "")
	if o.VersionID != "v2" || !o.IsLatest {
		t.Fatalf("latest should be v2: %+v", o)
	}
	o, err := db.GetObject(ctx, "b", "k", "v1")
	if err != nil || o.ETag != "e1" || o.IsLatest {
		t.Fatalf("v1: %+v %v", o, err)
	}
	if _, err := db.GetObject(ctx, "b", "k", "v9"); !errors.Is(err, ErrNoSuchVersion) {
		t.Fatalf("want NoSuchVersion got %v", err)
	}
	// Delete without version inserts a marker.
	r := mustApply(t, db, &DeleteObject{Bucket: "b", Key: "k", MarkerVersionID: "dm1", Now: 30})
	var dr DeleteObjectResult
	_ = r.Decode(&dr)
	if !dr.DeleteMarker || dr.VersionID != "dm1" {
		t.Fatalf("expected delete marker: %+v", dr)
	}
	o, _ = db.GetObject(ctx, "b", "k", "")
	if !o.DeleteMarker {
		t.Fatal("latest should be delete marker")
	}
	lr, _ := db.ListObjects(ctx, "b", ListOpts{})
	if len(lr.Objects) != 0 {
		t.Fatal("delete-marked key must not list")
	}
	vl, _ := db.ListVersions(ctx, "b", VersionListOpts{})
	if len(vl.Versions) != 3 || vl.Versions[0].VersionID != "dm1" || vl.Versions[2].VersionID != "v1" {
		t.Fatalf("versions newest first: %+v", vl.Versions)
	}
	// Remove the marker: v2 becomes latest again.
	mustApply(t, db, &DeleteObject{Bucket: "b", Key: "k", VersionID: "dm1", Now: 40})
	o, _ = db.GetObject(ctx, "b", "k", "")
	if o.VersionID != "v2" || o.DeleteMarker {
		t.Fatalf("v2 should be latest again: %+v", o)
	}
	// Delete the latest version: v1 promoted.
	mustApply(t, db, &DeleteObject{Bucket: "b", Key: "k", VersionID: "v2", Now: 50})
	o, _ = db.GetObject(ctx, "b", "k", "")
	if o.VersionID != "v1" {
		t.Fatalf("v1 should be promoted: %+v", o)
	}
	un, _ := db.UnreferencedChunks(ctx, 1<<62, 10)
	if len(un) != 1 || un[0].Hash != chunk(2).Hash {
		t.Fatalf("chunk 2 unreferenced: %+v", un)
	}
	// Deleting an unknown version is a no-op success.
	mustApply(t, db, &DeleteObject{Bucket: "b", Key: "k", VersionID: "zzz", Now: 51})

	// Suspended: null version replaces, others kept.
	mustApply(t, db, &SetBucketVersioning{Name: "b", State: VersioningSuspended})
	r = mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "k", VersionID: "v3", ETag: "e3", Now: 60})
	var pr PutObjectResult
	_ = r.Decode(&pr)
	if pr.VersionID != NullVersionID {
		t.Fatalf("suspended put must be null version: %q", pr.VersionID)
	}
	vl, _ = db.ListVersions(ctx, "b", VersionListOpts{})
	if len(vl.Versions) != 2 {
		t.Fatalf("expect null + v1: %+v", vl.Versions)
	}
	r = mustApply(t, db, &DeleteObject{Bucket: "b", Key: "k", Now: 70})
	_ = r.Decode(&dr)
	if !dr.DeleteMarker || dr.VersionID != NullVersionID {
		t.Fatalf("suspended delete inserts null marker: %+v", dr)
	}
	vl, _ = db.ListVersions(ctx, "b", VersionListOpts{})
	if len(vl.Versions) != 2 || !vl.Versions[0].DeleteMarker {
		t.Fatalf("null marker + v1: %+v", vl.Versions)
	}
}

func TestDeleteObjectsBatch(t *testing.T) {
	db := openTest(t)
	mustApply(t, db, &CreateBucket{Name: "b", Owner: "u", Now: 1})
	for i := 0; i < 3; i++ {
		mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: fmt.Sprint("k", i), VersionID: "x", Now: 1})
	}
	r := mustApply(t, db, &DeleteObjects{Bucket: "b", Now: 5, Items: []DeleteObject{{Key: "k0"}, {Key: "k1"}, {Key: "missing"}}})
	var res DeleteObjectsResult
	_ = r.Decode(&res)
	if len(res.Results) != 3 || !res.Results[0].Existed || res.Results[2].Existed || res.Results[2].Err != "" {
		t.Fatalf("batch: %+v", res)
	}
	lr, _ := db.ListObjects(context.Background(), "b", ListOpts{})
	if len(lr.Objects) != 1 || lr.Objects[0].Key != "k2" {
		t.Fatalf("remaining: %+v", lr.Objects)
	}
	applyErr(t, db, &DeleteObjects{Bucket: "nope", Items: []DeleteObject{{Key: "k"}}}, CodeNoSuchBucket)
}

func TestListObjects(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	mustApply(t, db, &CreateBucket{Name: "b", Owner: "u", Now: 1})
	keys := []string{"a.txt", "dir/one", "dir/sub/two", "dir/three", "e/x", "f.txt", "g/", "z"}
	for _, k := range keys {
		mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: k, VersionID: "x", Now: 1})
	}
	// No delimiter: all keys in order.
	r, err := db.ListObjects(ctx, "b", ListOpts{})
	if err != nil || len(r.Objects) != len(keys) || r.IsTruncated {
		t.Fatalf("all: %d %v", len(r.Objects), err)
	}
	// Delimiter at root.
	r, _ = db.ListObjects(ctx, "b", ListOpts{Delimiter: "/"})
	wantObjs := []string{"a.txt", "f.txt", "z"}
	wantCP := []string{"dir/", "e/", "g/"}
	if fmt.Sprint(keysOf(r.Objects)) != fmt.Sprint(wantObjs) || fmt.Sprint(r.CommonPrefixes) != fmt.Sprint(wantCP) {
		t.Fatalf("root delim: %v %v", keysOf(r.Objects), r.CommonPrefixes)
	}
	// Prefix + delimiter.
	r, _ = db.ListObjects(ctx, "b", ListOpts{Prefix: "dir/", Delimiter: "/"})
	if fmt.Sprint(keysOf(r.Objects)) != "[dir/one dir/three]" || fmt.Sprint(r.CommonPrefixes) != "[dir/sub/]" {
		t.Fatalf("dir delim: %v %v", keysOf(r.Objects), r.CommonPrefixes)
	}
	// Pagination with max 2 across objects and prefixes.
	var all []string
	cursor := ""
	for i := 0; i < 10; i++ {
		r, _ = db.ListObjects(ctx, "b", ListOpts{Delimiter: "/", Max: 2, StartAfter: cursor})
		all = append(all, keysOf(r.Objects)...)
		all = append(all, r.CommonPrefixes...)
		if !r.IsTruncated {
			break
		}
		cursor = r.NextCursor
	}
	if len(all) != 6 {
		t.Fatalf("paginated: %v", all)
	}
	// StartAfter.
	r, _ = db.ListObjects(ctx, "b", ListOpts{StartAfter: "dir/three"})
	if r.Objects[0].Key != "e/x" {
		t.Fatalf("start after: %v", keysOf(r.Objects))
	}
	// Prefix that matches nothing.
	r, _ = db.ListObjects(ctx, "b", ListOpts{Prefix: "nothing"})
	if len(r.Objects) != 0 {
		t.Fatal("expected empty")
	}
	// Exact max boundary is not truncated.
	r, _ = db.ListObjects(ctx, "b", ListOpts{Max: len(keys)})
	if r.IsTruncated {
		t.Fatal("exact max should not truncate")
	}
	r, _ = db.ListObjects(ctx, "b", ListOpts{Max: len(keys) - 1})
	if !r.IsTruncated {
		t.Fatal("max-1 should truncate")
	}
}

func keysOf(vs []ObjectVersion) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Key)
	}
	return out
}

func TestListVersionsPaging(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	mustApply(t, db, &CreateBucket{Name: "b", Owner: "u", Now: 1})
	mustApply(t, db, &SetBucketVersioning{Name: "b", State: VersioningEnabled})
	for i := 0; i < 3; i++ {
		mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "a", VersionID: fmt.Sprint("a", i), Now: 1})
		mustApply(t, db, &PutObjectVersion{Bucket: "b", Key: "d/b", VersionID: fmt.Sprint("b", i), Now: 1})
	}
	var got []string
	o := VersionListOpts{Max: 2}
	for i := 0; i < 10; i++ {
		r, err := db.ListVersions(ctx, "b", o)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range r.Versions {
			got = append(got, v.Key+":"+v.VersionID)
		}
		if !r.IsTruncated {
			break
		}
		o.KeyMarker, o.VersionIDMarker = r.NextKeyMarker, r.NextVersionIDMarker
	}
	want := "[a:a2 a:a1 a:a0 d/b:b2 d/b:b1 d/b:b0]"
	if fmt.Sprint(got) != want {
		t.Fatalf("got %v want %s", got, want)
	}
	r, _ := db.ListVersions(ctx, "b", VersionListOpts{Delimiter: "/"})
	if len(r.Versions) != 3 || fmt.Sprint(r.CommonPrefixes) != "[d/]" {
		t.Fatalf("delim: %d %v", len(r.Versions), r.CommonPrefixes)
	}
}

func TestMultipart(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	mustApply(t, db, &CreateBucket{Name: "b", Owner: "u", Now: 1})
	applyErr(t, db, &CreateUpload{UploadID: "u1", Bucket: "nope", Key: "k"}, CodeNoSuchBucket)
	mustApply(t, db, &CreateUpload{UploadID: "u1", Bucket: "b", Key: "k", ContentType: "x/y", Metadata: map[string]string{"m": "1"}, Now: 1})
	applyErr(t, db, &PutPart{UploadID: "zz", Number: 1}, CodeNoSuchUpload)
	applyErr(t, db, &PutPart{UploadID: "u1", Number: 0}, CodeInvalidArgument)
	mustApply(t, db, &PutPart{UploadID: "u1", Number: 1, Size: 6 << 20, ETag: "p1", Chunks: []ChunkRef{chunk(1)}, Now: 2})
	mustApply(t, db, &PutPart{UploadID: "u1", Number: 2, Size: 6 << 20, ETag: "p2", Chunks: []ChunkRef{chunk(2)}, Now: 2})
	// Re-upload part 1 with different content releases chunk 1.
	mustApply(t, db, &PutPart{UploadID: "u1", Number: 1, Size: 6 << 20, ETag: "p1b", Chunks: []ChunkRef{chunk(3)}, Now: 3})
	mustApply(t, db, &PutPart{UploadID: "u1", Number: 3, Size: 10, ETag: "p3", Chunks: []ChunkRef{chunk(4)}, Now: 3})
	parts, more, _ := db.ListParts(ctx, "u1", 0, 2)
	if len(parts) != 2 || !more || parts[0].ETag != "p1b" {
		t.Fatalf("list parts: %+v %v", parts, more)
	}
	parts, more, _ = db.ListParts(ctx, "u1", 2, 10)
	if len(parts) != 1 || more || parts[0].Number != 3 {
		t.Fatalf("list parts marker: %+v", parts)
	}
	ul, _ := db.ListUploads(ctx, "b", UploadListOpts{})
	if len(ul.Uploads) != 1 || ul.Uploads[0].Key != "k" {
		t.Fatalf("list uploads: %+v", ul)
	}
	applyErr(t, db, &CompleteUpload{UploadID: "u1", VersionID: "v", Parts: []CompletePart{{2, "p2"}, {1, "p1b"}}}, CodeInvalidPartOrder)
	applyErr(t, db, &CompleteUpload{UploadID: "u1", VersionID: "v", Parts: []CompletePart{{1, "wrong"}}}, CodeInvalidPart)
	applyErr(t, db, &CompleteUpload{UploadID: "u1", VersionID: "v", Parts: []CompletePart{{1, "p1b"}, {4, "x"}}}, CodeInvalidPart)
	// Small part not last.
	mustApply(t, db, &PutPart{UploadID: "u1", Number: 2, Size: 10, ETag: "small", Chunks: []ChunkRef{chunk(2)}, Now: 3})
	applyErr(t, db, &CompleteUpload{UploadID: "u1", VersionID: "v", Parts: []CompletePart{{2, "small"}, {3, "p3"}}}, CodeEntityTooSmall)
	r := mustApply(t, db, &CompleteUpload{UploadID: "u1", VersionID: "v", ETag: "final-2", Parts: []CompletePart{{1, `"p1b"`}, {3, "p3"}}, Now: 9, ExtraMetadata: map[string]string{"mp": "x"}})
	var cr CompleteUploadResult
	_ = r.Decode(&cr)
	if cr.Size != (6<<20)+10 || cr.VersionID != NullVersionID {
		t.Fatalf("complete: %+v", cr)
	}
	o, err := db.GetObject(ctx, "b", "k", "")
	if err != nil || len(o.Chunks) != 2 || o.Chunks[0] != chunk(3) || o.Chunks[1] != chunk(4) || o.ContentType != "x/y" || o.Metadata["m"] != "1" || o.Metadata["mp"] != "x" {
		t.Fatalf("assembled object: %+v %v", o, err)
	}
	mustApply(t, db, &UpdateObjectMeta{Bucket: "b", Key: "k", Set: map[string]string{"tag": "v"}, Remove: []string{"m"}})
	o, _ = db.GetObject(ctx, "b", "k", "")
	if o.Metadata["tag"] != "v" || o.Metadata["m"] != "" || o.Metadata["mp"] != "x" {
		t.Fatalf("update meta: %+v", o.Metadata)
	}
	applyErr(t, db, &UpdateObjectMeta{Bucket: "b", Key: "nope"}, CodeNoSuchKey)
	applyErr(t, db, &UpdateObjectMeta{Bucket: "b", Key: "k", VersionID: "zz"}, CodeNoSuchVersion)
	if _, err := db.GetUpload(ctx, "u1"); !errors.Is(err, ErrNoSuchUpload) {
		t.Fatal("upload should be gone after complete")
	}
	// Chunks 1 and 2 (unused part content) are unreferenced; 3 and 4 referenced.
	un, _ := db.UnreferencedChunks(ctx, 1<<62, 10)
	if len(un) != 2 {
		t.Fatalf("unreferenced after complete: %+v", un)
	}
	// Abort path.
	mustApply(t, db, &CreateUpload{UploadID: "u2", Bucket: "b", Key: "k2", Now: 1})
	mustApply(t, db, &PutPart{UploadID: "u2", Number: 1, Size: 1, ETag: "x", Chunks: []ChunkRef{chunk(5)}, Now: 2})
	mustApply(t, db, &AbortUpload{UploadID: "u2", Now: 4})
	applyErr(t, db, &AbortUpload{UploadID: "u2"}, CodeNoSuchUpload)
	un, _ = db.UnreferencedChunks(ctx, 1<<62, 10)
	if len(un) != 3 {
		t.Fatalf("unreferenced after abort: %+v", un)
	}
	exp, _ := db.ExpiredUploads(ctx, 1<<62, 10)
	if len(exp) != 0 {
		t.Fatal("no uploads should remain")
	}
}

func TestUsersAndKeys(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	applyErr(t, db, &CreateUser{ID: "u1", Name: "alice", Role: "god"}, CodeInvalidArgument)
	mustApply(t, db, &CreateUser{ID: "u1", Name: "alice", Role: RoleAdmin, Now: 1})
	applyErr(t, db, &CreateUser{ID: "u2", Name: "alice", Role: RoleAdmin}, CodeUserExists)
	mustApply(t, db, &CreateUser{ID: "u2", Name: "bob", Role: RoleReadOnly, Buckets: []string{"b1"}, Now: 1})
	applyErr(t, db, &CreateAccessKey{AccessKeyID: "AK", Secret: "SK", UserID: "nope"}, CodeNoSuchUser)
	mustApply(t, db, &CreateAccessKey{AccessKeyID: "AK1", Secret: "SK1", UserID: "u1", Label: "laptop", Now: 2})
	mustApply(t, db, &CreateAccessKey{AccessKeyID: "AK2", Secret: "SK2", UserID: "u2", Now: 2})
	c, err := db.LookupAccessKey(ctx, "AK2")
	if err != nil || c.User.Name != "bob" || c.Key.Secret != "SK2" || c.User.Buckets[0] != "b1" {
		t.Fatalf("lookup: %+v %v", c, err)
	}
	if _, err := db.LookupAccessKey(ctx, "zz"); !errors.Is(err, ErrNoSuchKeyID) {
		t.Fatal("want NoSuchAccessKey")
	}
	mustApply(t, db, &UpdateAccessKey{AccessKeyID: "AK2", Disabled: true})
	c, _ = db.LookupAccessKey(ctx, "AK2")
	if !c.Key.Disabled {
		t.Fatal("key should be disabled")
	}
	role := RoleReadWrite
	dis := true
	mustApply(t, db, &UpdateUser{ID: "u2", Role: &role, Disabled: &dis, Buckets: &[]string{}})
	u, _ := db.GetUser(ctx, "u2")
	if u.Role != RoleReadWrite || !u.Disabled || len(u.Buckets) != 0 {
		t.Fatalf("update user: %+v", u)
	}
	if _, err := db.GetUserByName(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	users, _ := db.ListUsers(ctx)
	if len(users) != 2 {
		t.Fatal("two users")
	}
	keys, _ := db.ListAccessKeys(ctx, "u1")
	if len(keys) != 1 || keys[0].Label != "laptop" {
		t.Fatalf("keys: %+v", keys)
	}
	mustApply(t, db, &DeleteAccessKey{AccessKeyID: "AK1"})
	applyErr(t, db, &DeleteAccessKey{AccessKeyID: "AK1"}, CodeNoSuchKeyID)
	mustApply(t, db, &DeleteUser{ID: "u2"})
	applyErr(t, db, &DeleteUser{ID: "u2"}, CodeNoSuchUser)
	if _, err := db.LookupAccessKey(ctx, "AK2"); !errors.Is(err, ErrNoSuchKeyID) {
		t.Fatal("keys must cascade on user delete")
	}
}

func TestSettingsNodesCertsAudit(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	mustApply(t, db, &SetSetting{Key: "rf", Value: "3"})
	v, _ := db.GetSetting(ctx, "rf")
	if v != "3" {
		t.Fatal("setting")
	}
	mustApply(t, db, &SetSetting{Key: "rf"})
	v, _ = db.GetSetting(ctx, "rf")
	all, _ := db.AllSettings(ctx)
	if v != "" || len(all) != 0 {
		t.Fatal("setting delete")
	}
	mustApply(t, db, &PutCert{Key: "certs/a/cert.pem", Value: []byte("A"), Now: 1})
	mustApply(t, db, &PutCert{Key: "certs/a/key.pem", Value: []byte("K"), Now: 1})
	mustApply(t, db, &PutCert{Key: "certs/b/cert.pem", Value: []byte("B"), Now: 1})
	val, _, err := db.GetCert(ctx, "certs/a/cert.pem")
	if err != nil || string(val) != "A" {
		t.Fatal("cert get")
	}
	l, _ := db.ListCerts(ctx, "certs", false)
	if fmt.Sprint(l) != "[certs/a certs/b]" {
		t.Fatalf("cert list shallow: %v", l)
	}
	l, _ = db.ListCerts(ctx, "certs", true)
	if len(l) != 3 {
		t.Fatalf("cert list deep: %v", l)
	}
	mustApply(t, db, &DeleteCert{Key: "certs/a", Prefix: true})
	l, _ = db.ListCerts(ctx, "certs", true)
	if len(l) != 1 {
		t.Fatalf("after prefix delete: %v", l)
	}

	mustApply(t, db, &Audit{Actor: "alice", Action: "user.create", Target: "bob", Now: 5})
	mustApply(t, db, &Audit{Actor: "alice", Action: "key.create", Target: "bob", Now: 6})
	entries, _ := db.ListAudit(ctx, 1, 0)
	if len(entries) != 1 || entries[0].Action != "key.create" {
		t.Fatalf("audit: %+v", entries)
	}
	entries, _ = db.ListAudit(ctx, 10, entries[0].ID)
	if len(entries) != 1 || entries[0].Action != "user.create" {
		t.Fatalf("audit paging: %+v", entries)
	}
	st, _ := db.Stats(ctx)
	if st == nil {
		t.Fatal("stats")
	}
}

func TestBatchCommand(t *testing.T) {
	db := openTest(t)
	c1, _ := Encode(&CreateBucket{Name: "b", Owner: "u", Now: 1})
	c2, _ := Encode(&SetBucketVersioning{Name: "b", State: VersioningEnabled})
	mustApply(t, db, &Batch{Commands: []json.RawMessage{c1, c2}})
	b, _ := db.GetBucket(context.Background(), "b")
	if b.Versioning != VersioningEnabled {
		t.Fatal("batch did not apply both")
	}
	// A failing batch rolls back entirely.
	c3, _ := Encode(&CreateBucket{Name: "c", Owner: "u", Now: 1})
	c4, _ := Encode(&DeleteBucket{Name: "nope"})
	applyErr(t, db, &Batch{Commands: []json.RawMessage{c3, c4}}, CodeNoSuchBucket)
	if _, err := db.GetBucket(context.Background(), "c"); err == nil {
		t.Fatal("batch should roll back")
	}
}

func TestReplayIdempotentAndDeterministic(t *testing.T) {
	ctx := context.Background()
	var log [][]byte
	add := func(c Command) {
		b, err := Encode(c)
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, b)
	}
	add(&CreateBucket{Name: "b", Owner: "u", Now: 1})
	add(&SetBucketVersioning{Name: "b", State: VersioningEnabled})
	add(&PutObjectVersion{Bucket: "b", Key: "k", VersionID: "v1", ETag: "e1", Chunks: []ChunkRef{chunk(1)}, Now: 2})
	add(&PutObjectVersion{Bucket: "b", Key: "k", VersionID: "v2", ETag: "e2", Chunks: []ChunkRef{chunk(2)}, Now: 3})
	add(&DeleteObject{Bucket: "b", Key: "k", MarkerVersionID: "dm", Now: 4})
	add(&DeleteBucket{Name: "b"}) // fails: not empty, must still advance index
	add(&CreateUser{ID: "u", Name: "u", Role: RoleAdmin, Now: 5})

	run := func(db *DB, from int) {
		for i := from; i < len(log); i++ {
			db.Apply(uint64(i+1), log[i])
		}
	}
	a := openTest(t)
	run(a, 0)
	idx, _ := a.AppliedIndex(ctx)
	if idx != uint64(len(log)) {
		t.Fatalf("applied index %d want %d", idx, len(log))
	}
	// Replaying the whole log again is a no-op.
	run(a, 0)
	// Applying the same entry twice does nothing.
	r := a.Apply(3, log[2])
	if r.Err != "" || len(r.Value) != 0 {
		t.Fatalf("replayed entry should be skipped: %+v", r)
	}

	b := openTest(t)
	run(b, 0)
	va, _ := a.ListVersions(ctx, "b", VersionListOpts{})
	vb, _ := b.ListVersions(ctx, "b", VersionListOpts{})
	if fmt.Sprint(va.Versions) != fmt.Sprint(vb.Versions) {
		t.Fatalf("two nodes diverged:\n%+v\n%+v", va.Versions, vb.Versions)
	}
	ua, _ := a.ListUsers(ctx)
	ub, _ := b.ListUsers(ctx)
	if len(ua) != 1 || len(ub) != 1 {
		t.Fatal("user command after failed command must still apply")
	}
	// Index 0 applies without tracking.
	if r := a.ApplyCommand(0, &SetSetting{Key: "x", Value: "1"}); r.Err != "" {
		t.Fatal(r.Msg)
	}
	idx, _ = a.AppliedIndex(ctx)
	if idx != uint64(len(log)) {
		t.Fatal("index 0 must not move applied index")
	}
}

func TestSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, err := Open(ctx, filepath.Join(dir, "a.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 5; i++ {
		b, _ := Encode(&CreateBucket{Name: fmt.Sprint("b", i), Owner: "u", Now: 1})
		a.Apply(uint64(i+1), b)
	}
	snap := filepath.Join(dir, "snap.db")
	if err := a.Snapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, filepath.Join(dir, "b.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	x, _ := Encode(&CreateBucket{Name: "stale", Owner: "u", Now: 1})
	b.Apply(1, x)
	if err := b.Restore(ctx, snap); err != nil {
		t.Fatal(err)
	}
	list, _ := b.ListBuckets(ctx, "")
	if len(list) != 5 || list[0].Name != "b0" {
		t.Fatalf("restored: %+v", list)
	}
	idx, _ := b.AppliedIndex(ctx)
	if idx != 5 {
		t.Fatalf("restored index %d", idx)
	}
	// Restored db keeps working.
	y, _ := Encode(&CreateBucket{Name: "after", Owner: "u", Now: 1})
	if r := b.Apply(6, y); r.Err != "" {
		t.Fatal(r.Msg)
	}
}

func TestPrefixHelpers(t *testing.T) {
	if prefixEnd("abc") != "abd" || prefixEnd("") != "" || prefixEnd("a\xff") != "b" || prefixEnd("\xff") != "" {
		t.Fatal("prefixEnd")
	}
	if likeEscape("a%b_c\\") != `a\%b\_c\\` {
		t.Fatal("likeEscape")
	}
}
