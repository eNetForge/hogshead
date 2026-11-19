package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func req(method, host, path string) *http.Request {
	r := httptest.NewRequest(method, "http://"+host+path, nil)
	r.Host = host
	return r
}

func TestResolveTarget(t *testing.T) {
	hosts := []string{"s3.example.com"}
	cases := []struct {
		host, path  string
		bucket, key string
		vhost       bool
	}{
		{"s3.example.com", "/", "", "", false},
		{"s3.example.com", "/b", "b", "", false},
		{"s3.example.com", "/b/", "b", "", false},
		{"s3.example.com", "/b/k/with%20space", "b", "k/with space", false},
		{"s3.example.com:9000", "/b/a+b", "b", "a+b", false},
		{"my-bucket.s3.example.com", "/k", "my-bucket", "k", true},
		{"my-bucket.s3.example.com:9000", "/", "my-bucket", "", true},
		{"MY-BUCKET.S3.EXAMPLE.COM", "/k", "my-bucket", "k", true},
		{"dotted.name.s3.example.com", "/k", "dotted.name", "k", true},
		{"other.com", "/b/k", "b", "k", false},
		{"127.0.0.1:9000", "/b/%E2%9C%93", "b", "✓", false},
	}
	for _, c := range cases {
		got := ResolveTarget(req("GET", c.host, c.path), hosts)
		if got.Bucket != c.bucket || got.Key != c.key || got.VirtualHost != c.vhost {
			t.Errorf("%s%s: got %+v", c.host, c.path, got)
		}
	}
	if got := ResolveTarget(req("GET", "b.s3.example.com", "/k"), nil); got.Bucket != "k" {
		t.Fatal("without hostnames only path style applies")
	}
}

func TestValidBucketName(t *testing.T) {
	good := []string{"abc", "my-bucket", "a.b.c", "123", "a1-b2.c3"}
	bad := []string{"ab", "Abc", "a_b", "-abc", "abc-", "a..b", "a.-b", "192.168.1.1", "xn--foo", "sthree-x", "foo-s3alias", "x--ol-s3", string(make([]byte, 64))}
	for _, g := range good {
		if !ValidBucketName(g) {
			t.Errorf("%q should be valid", g)
		}
	}
	for _, b := range bad {
		if ValidBucketName(b) {
			t.Errorf("%q should be invalid", b)
		}
	}
	if ValidKey("") || !ValidKey("a") || ValidKey(string(make([]byte, 1025))) {
		t.Fatal("ValidKey")
	}
}

func TestRoute(t *testing.T) {
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	b := Target{Bucket: "b"}
	o := Target{Bucket: "b", Key: "k"}
	copyHdr := http.Header{"X-Amz-Copy-Source": []string{"/a/b"}}
	cases := []struct {
		m    string
		t    Target
		q    url.Values
		want Operation
	}{
		{"GET", Target{}, nil, OpListBuckets},
		{"PUT", Target{}, nil, OpUnknown},
		{"OPTIONS", o, nil, OpOptions},
		{"GET", b, nil, OpListObjects},
		{"GET", b, q("list-type=2&prefix=x"), OpListObjectsV2},
		{"GET", b, q("versions"), OpListObjectVersions},
		{"GET", b, q("uploads"), OpListMultipartUploads},
		{"GET", b, q("location"), OpGetBucketLocation},
		{"GET", b, q("versioning"), OpGetBucketVersioning},
		{"GET", b, q("policy"), OpGetBucketPolicy},
		{"GET", b, q("policyStatus"), OpGetBucketPolicyStatus},
		{"GET", b, q("cors"), OpGetBucketCors},
		{"GET", b, q("lifecycle"), OpGetBucketLifecycle},
		{"GET", b, q("acl"), OpGetBucketAcl},
		{"GET", b, q("tagging"), OpGetBucketTagging},
		{"GET", b, q("encryption"), OpGetBucketEncryption},
		{"GET", b, q("object-lock"), OpGetObjectLockConfig},
		{"GET", b, q("publicAccessBlock"), OpGetPublicAccessBlock},
		{"GET", b, q("ownershipControls"), OpGetBucketOwnership},
		{"GET", b, q("logging"), OpGetBucketLogging},
		{"GET", b, q("notification"), OpGetBucketNotification},
		{"GET", b, q("replication"), OpGetBucketReplication},
		{"PUT", b, nil, OpCreateBucket},
		{"PUT", b, q("versioning"), OpPutBucketVersioning},
		{"PUT", b, q("policy"), OpPutBucketPolicy},
		{"PUT", b, q("cors"), OpPutBucketCors},
		{"PUT", b, q("lifecycle"), OpPutBucketLifecycle},
		{"PUT", b, q("acl"), OpPutBucketAcl},
		{"PUT", b, q("tagging"), OpPutBucketTagging},
		{"PUT", b, q("encryption"), OpPutBucketEncryption},
		{"PUT", b, q("object-lock"), OpPutObjectLockConfig},
		{"PUT", b, q("publicAccessBlock"), OpPutPublicAccessBlock},
		{"PUT", b, q("ownershipControls"), OpPutBucketOwnership},
		{"PUT", b, q("notification"), OpPutBucketNotification},
		{"DELETE", b, nil, OpDeleteBucket},
		{"DELETE", b, q("policy"), OpDeleteBucketPolicy},
		{"DELETE", b, q("cors"), OpDeleteBucketCors},
		{"DELETE", b, q("lifecycle"), OpDeleteBucketLifecycle},
		{"DELETE", b, q("tagging"), OpDeleteBucketTagging},
		{"DELETE", b, q("encryption"), OpDeleteBucketEncryption},
		{"DELETE", b, q("publicAccessBlock"), OpDeletePublicAccessBlock},
		{"HEAD", b, nil, OpHeadBucket},
		{"POST", b, q("delete"), OpDeleteObjects},
		{"POST", b, nil, OpPostObject},
		{"PATCH", b, nil, OpUnknown},
		{"GET", o, nil, OpGetObject},
		{"GET", o, q("uploadId=1"), OpListParts},
		{"GET", o, q("acl"), OpGetObjectAcl},
		{"GET", o, q("tagging"), OpGetObjectTagging},
		{"GET", o, q("retention"), OpGetObjectRetention},
		{"GET", o, q("legal-hold"), OpGetObjectLegalHold},
		{"GET", o, q("attributes"), OpGetObjectAttributes},
		{"HEAD", o, nil, OpHeadObject},
		{"PUT", o, nil, OpPutObject},
		{"PUT", o, WithHeaders(nil, copyHdr), OpCopyObject},
		{"PUT", o, q("uploadId=1&partNumber=2"), OpUploadPart},
		{"PUT", o, WithHeaders(q("uploadId=1&partNumber=2"), copyHdr), OpUploadPartCopy},
		{"PUT", o, q("acl"), OpPutObjectAcl},
		{"PUT", o, q("tagging"), OpPutObjectTagging},
		{"PUT", o, q("retention"), OpPutObjectRetention},
		{"PUT", o, q("legal-hold"), OpPutObjectLegalHold},
		{"POST", o, q("uploads"), OpCreateMultipart},
		{"POST", o, q("uploadId=1"), OpCompleteMultipart},
		{"POST", o, nil, OpUnknown},
		{"DELETE", o, nil, OpDeleteObject},
		{"DELETE", o, q("uploadId=1"), OpAbortMultipart},
		{"DELETE", o, q("tagging"), OpDeleteObjectTagging},
		{"PATCH", o, nil, OpUnknown},
	}
	for _, c := range cases {
		if got := Route(c.m, c.t, c.q); got != c.want {
			t.Errorf("%s %+v %v: got %s want %s", c.m, c.t, c.q, got, c.want)
		}
	}
	// Every operation maps to an s3: action.
	for _, op := range []Operation{OpListBuckets, OpCreateBucket, OpGetObject, OpPutObject, OpListParts, OpGetBucketCors, Operation("Weird")} {
		if a := ActionFor(op); len(a) < 4 || a[:3] != "s3:" {
			t.Errorf("%s -> %q", op, a)
		}
	}
	if WithHeaders(q("a=1"), http.Header{}).Get("a") != "1" {
		t.Fatal("WithHeaders copies")
	}
}

func TestMetadata(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amz-Meta-Foo", "bar")
	h.Add("x-amz-meta-multi", "a")
	h.Add("x-amz-meta-multi", "b")
	h.Set("Content-Type", "text/plain")
	h.Set("Cache-Control", "max-age=1")
	h.Set("Authorization", "secret")
	m, err := ExtractMetadata(h)
	if err != nil || m["x-amz-meta-foo"] != "bar" || m["x-amz-meta-multi"] != "a,b" || m["content-type"] != "text/plain" || m["cache-control"] != "max-age=1" || m["authorization"] != "" {
		t.Fatalf("%v %v", m, err)
	}
	out := http.Header{}
	ApplyMetadata(out, m)
	if out["x-amz-meta-foo"][0] != "bar" || out.Get("Content-Type") != "text/plain" {
		t.Fatal("apply")
	}
	if _, canonical := out["X-Amz-Meta-Foo"]; canonical {
		t.Fatal("user metadata must be emitted lower-case")
	}
	big := http.Header{}
	big.Set("X-Amz-Meta-Big", string(make([]byte, 3000)))
	if _, err := ExtractMetadata(big); err != ErrMetadataTooLarge {
		t.Fatal("size limit")
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		h           string
		off, length int64
		ok, err     bool
	}{
		{"", 0, 100, false, false},
		{"bytes=0-9", 0, 10, true, false},
		{"bytes=90-", 90, 10, true, false},
		{"bytes=-10", 90, 10, true, false},
		{"bytes=-200", 0, 100, true, false},
		{"bytes=50-200", 50, 50, true, false},
		{"bytes=100-", 0, 0, true, true},
		{"bytes=-0", 0, 0, true, true},
		{"bytes=5-3", 0, 100, false, false},
		{"bytes=a-b", 0, 0, true, true},
		{"items=0-1", 0, 100, false, false},
		{"bytes=0-1,5-6", 0, 100, false, false},
		{"bytes=-", 0, 100, false, false},
		{"bytes=0", 0, 100, false, false},
	}
	for _, c := range cases {
		off, length, ok, err := ParseRange(c.h, 100)
		if ok != c.ok || (err != nil) != c.err || (ok && err == nil && (off != c.off || length != c.length)) {
			t.Errorf("%q: off=%d len=%d ok=%v err=%v", c.h, off, length, ok, err)
		}
	}
	if ContentRange(10, 5, 100) != "bytes 10-14/100" {
		t.Fatal("content range")
	}
	if _, _, _, err := ParseRange("bytes=-5", 0); err == nil {
		t.Fatal("suffix on empty object")
	}
}

func TestConditions(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	etag := `"abc"`
	mk := func(k, v string) http.Header { h := http.Header{}; h.Set(k, v); return h }
	cases := []struct {
		h    http.Header
		want Conditional
	}{
		{http.Header{}, CondOK},
		{mk("If-Match", `"abc"`), CondOK},
		{mk("If-Match", `abc`), CondOK},
		{mk("If-Match", `W/"abc"`), CondOK},
		{mk("If-Match", `"x", "abc"`), CondOK},
		{mk("If-Match", `*`), CondOK},
		{mk("If-Match", `"nope"`), CondPreconditionFailed},
		{mk("If-None-Match", `"abc"`), CondNotModified},
		{mk("If-None-Match", `*`), CondNotModified},
		{mk("If-None-Match", `"nope"`), CondOK},
		{mk("If-Modified-Since", FormatHTTPTime(now)), CondNotModified},
		{mk("If-Modified-Since", FormatHTTPTime(now.Add(-time.Hour))), CondOK},
		{mk("If-Modified-Since", "garbage"), CondOK},
		{mk("If-Unmodified-Since", FormatHTTPTime(now)), CondOK},
		{mk("If-Unmodified-Since", FormatHTTPTime(now.Add(-time.Hour))), CondPreconditionFailed},
	}
	for i, c := range cases {
		if got := EvaluateConditions(c.h, etag, now); got != c.want {
			t.Errorf("case %d %v: got %v want %v", i, c.h, got, c.want)
		}
	}
	// If-Match takes precedence over If-Unmodified-Since; If-None-Match over If-Modified-Since.
	h := mk("If-Match", `"abc"`)
	h.Set("If-Unmodified-Since", FormatHTTPTime(now.Add(-time.Hour)))
	if EvaluateConditions(h, etag, now) != CondOK {
		t.Fatal("precedence")
	}
	if QuoteETag("x") != `"x"` || QuoteETag(`"x"`) != `"x"` {
		t.Fatal("quote")
	}
}

func TestCopySourceAndParseInt(t *testing.T) {
	b, k, v, err := CopySource("/src-bucket/path/to%20key?versionId=abc")
	if err != nil || b != "src-bucket" || k != "path/to key" || v != "abc" {
		t.Fatalf("%s %s %s %v", b, k, v, err)
	}
	if _, _, _, err := CopySource("nokey"); err == nil {
		t.Fatal("invalid copy source")
	}
	if n, err := ParseInt("", 7, 0, 10); err != nil || n != 7 {
		t.Fatal("default")
	}
	if n, _ := ParseInt("50", 7, 0, 10); n != 10 {
		t.Fatal("clamp")
	}
	if _, err := ParseInt("-1", 7, 0, 10); err == nil {
		t.Fatal("min")
	}
	if _, err := ParseInt("x", 7, 0, 10); err == nil {
		t.Fatal("nan")
	}
}
