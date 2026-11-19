package usage

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"GetObject": ClassRead, "HeadObject": ClassRead, "GetObjectTagging": ClassRead,
		"PutObject": ClassWrite, "CopyObject": ClassWrite, "CompleteMultipartUpload": ClassWrite, "UploadPart": ClassWrite, "PutObjectRetention": ClassWrite,
		"DeleteObject": ClassDelete, "DeleteObjects": ClassDelete, "AbortMultipartUpload": ClassDelete,
		"ListObjectsV2": ClassList, "ListBuckets": ClassList, "ListMultipartUploads": ClassList,
		"CreateBucket": ClassBucket, "PutBucketPolicy": ClassBucket, "GetBucketVersioning": ClassBucket, "DeleteBucket": ClassBucket,
		"Options": ClassOther, "": ClassOther,
	}
	for op, want := range cases {
		if got := Classify(op); got != want {
			t.Errorf("%s: got %s want %s", op, got, want)
		}
	}
}

func TestAggregateFlushAndLog(t *testing.T) {
	var got []meta.UsageRow
	fail := false
	propose := func(_ context.Context, cmd meta.Command) (meta.Result, error) {
		if fail {
			return meta.Result{Err: "boom"}, nil
		}
		got = append(got, cmd.(*meta.AddUsage).Rows...)
		return meta.Result{}, nil
	}
	s := New(propose, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "access.log")
	s.SetAccessLog(path)
	at := time.Date(2026, 11, 18, 10, 15, 0, 0, time.UTC)
	s.Handle(Record{Time: at, Op: "GetObject", Bucket: "b", User: "alice", AccessKey: "AK1", Status: 200, BytesOut: 100, IP: "1.2.3.4"})
	s.Handle(Record{Time: at.Add(time.Minute), Op: "GetObject", Bucket: "b", User: "alice", AccessKey: "AK1", Status: 404, BytesOut: 0, IP: "1.2.3.4"})
	s.Handle(Record{Time: at, Op: "PutObject", Bucket: "b", User: "alice", AccessKey: "AK1", Status: 200, BytesIn: 50, IP: "1.2.3.4"})
	s.Handle(Record{Time: at.Add(time.Hour), Op: "ListObjectsV2", Bucket: "b", User: "bob", AccessKey: "AK2", Status: 200, IP: "5.6.7.8"})
	// A failed flush keeps the counters; the next flush delivers them once.
	fail = true
	if err := s.Flush(context.Background()); err == nil {
		t.Fatal("expected flush error")
	}
	fail = false
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rows: %+v", got)
	}
	byKey := map[string]meta.UsageRow{}
	for _, r := range got {
		byKey[r.User+"/"+r.Class] = r
	}
	if r := byKey["alice/read"]; r.Requests != 2 || r.Errors != 1 || r.BytesOut != 100 || r.Hour != at.Truncate(time.Hour).Unix() {
		t.Fatalf("alice/read: %+v", r)
	}
	if r := byKey["alice/write"]; r.Requests != 1 || r.BytesIn != 50 {
		t.Fatalf("alice/write: %+v", r)
	}
	if r := byKey["bob/list"]; r.Requests != 1 || r.Hour != at.Add(time.Hour).Truncate(time.Hour).Unix() {
		t.Fatalf("bob/list: %+v", r)
	}
	if err := s.Flush(context.Background()); err != nil || len(got) != 3 {
		t.Fatal("second flush must be empty")
	}
	// The access log holds one JSON line per request after draining.
	s.drain()
	s.SetAccessLog("") // closes the file
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines++
		if !strings.Contains(sc.Text(), `"op":"`) || !strings.HasPrefix(sc.Text(), "{") {
			t.Fatalf("bad line: %s", sc.Text())
		}
	}
	if lines != 4 {
		t.Fatalf("access log lines: %d", lines)
	}
}
