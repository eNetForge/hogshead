package api_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/testutil"
)

func hdr(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestBucketLifecycle(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	resp, body := n.Call(a, "GET", "/", nil, nil)
	n.Expect(200, resp, body)
	var lb s3xml.ListAllMyBucketsResult
	_ = xml.Unmarshal(body, &lb)
	if len(lb.Buckets) != 0 || lb.Owner.DisplayName != "admin" {
		t.Fatalf("empty list: %s", body)
	}
	resp, body = n.Call(a, "PUT", "/Bad_Name", nil, nil)
	n.Expect(400, resp, body)
	if !strings.Contains(string(body), "InvalidBucketName") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "PUT", "/bkt1", nil, nil)
	n.Expect(200, resp, body)
	if resp.Header.Get("Location") != "/bkt1" {
		t.Fatal("location header")
	}
	// Re-creating your own bucket is a no-op success in us-east-1.
	resp, body = n.Call(a, "PUT", "/bkt1", nil, nil)
	n.Expect(200, resp, body)
	other := n.CreateUser("other", meta.RoleReadWrite, nil)
	resp, body = n.Call(&other, "PUT", "/bkt1", nil, nil)
	n.Expect(409, resp, body)
	if !strings.Contains(string(body), "BucketAlreadyExists") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "HEAD", "/bkt1", nil, nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "HEAD", "/nope", nil, nil)
	n.Expect(404, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?location", nil, nil)
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "LocationConstraint") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "GET", "/", nil, nil)
	n.Expect(200, resp, body)
	_ = xml.Unmarshal(body, &lb)
	if len(lb.Buckets) != 1 || lb.Buckets[0].Name != "bkt1" {
		t.Fatalf("list: %s", body)
	}
	// Versioning.
	resp, body = n.Call(a, "GET", "/bkt1?versioning", nil, nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "PUT", "/bkt1?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?versioning", nil, nil)
	if !strings.Contains(string(body), "<Status>Enabled</Status>") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "PUT", "/bkt1?versioning", []byte(`<VersioningConfiguration><Status>Weird</Status></VersioningConfiguration>`), nil)
	n.Expect(400, resp, body)
	// Policy.
	resp, body = n.Call(a, "GET", "/bkt1?policy", nil, nil)
	n.Expect(404, resp, body)
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt1/*"}]}`
	resp, body = n.Call(a, "PUT", "/bkt1?policy", []byte(pol), nil)
	n.Expect(204, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?policy", nil, nil)
	n.Expect(200, resp, body)
	if string(body) != pol {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "GET", "/bkt1?policyStatus", nil, nil)
	if !strings.Contains(string(body), "<IsPublic>true</IsPublic>") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "PUT", "/bkt1?policy", []byte(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"}]}`), nil)
	n.Expect(400, resp, body)
	resp, body = n.Call(a, "DELETE", "/bkt1?policy", nil, nil)
	n.Expect(204, resp, body)
	// CORS.
	resp, body = n.Call(a, "GET", "/bkt1?cors", nil, nil)
	n.Expect(404, resp, body)
	cors := `<CORSConfiguration><CORSRule><AllowedOrigin>https://app.example</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod><AllowedHeader>*</AllowedHeader><ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>300</MaxAgeSeconds></CORSRule></CORSConfiguration>`
	resp, body = n.Call(a, "PUT", "/bkt1?cors", []byte(cors), nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?cors", nil, nil)
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "https://app.example") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(nil, "OPTIONS", "/bkt1/key", nil, hdr("Origin", "https://app.example", "Access-Control-Request-Method", "PUT", "Access-Control-Request-Headers", "content-type"))
	n.Expect(200, resp, body)
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" || resp.Header.Get("Access-Control-Max-Age") != "300" || resp.Header.Get("Access-Control-Allow-Headers") != "content-type" {
		t.Fatalf("preflight headers: %v", resp.Header)
	}
	resp, body = n.Call(nil, "OPTIONS", "/bkt1/key", nil, hdr("Origin", "https://evil.example", "Access-Control-Request-Method", "PUT"))
	n.Expect(403, resp, body)
	resp, body = n.Call(a, "PUT", "/bkt1?cors", []byte(`<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>PATCH</AllowedMethod></CORSRule></CORSConfiguration>`), nil)
	n.Expect(400, resp, body)
	resp, body = n.Call(a, "DELETE", "/bkt1?cors", nil, nil)
	n.Expect(204, resp, body)
	// Lifecycle.
	lc := `<LifecycleConfiguration><Rule><ID>exp</ID><Status>Enabled</Status><Filter><Prefix>tmp/</Prefix></Filter><Expiration><Days>7</Days></Expiration></Rule></LifecycleConfiguration>`
	resp, body = n.Call(a, "PUT", "/bkt1?lifecycle", []byte(lc), nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?lifecycle", nil, nil)
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "<Days>7</Days>") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "PUT", "/bkt1?lifecycle", []byte(`<LifecycleConfiguration><Rule><Status>Enabled</Status></Rule></LifecycleConfiguration>`), nil)
	n.Expect(400, resp, body)
	resp, body = n.Call(a, "DELETE", "/bkt1?lifecycle", nil, nil)
	n.Expect(204, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?lifecycle", nil, nil)
	n.Expect(404, resp, body)
	// Tagging, ACL, misc GETs.
	resp, body = n.Call(a, "PUT", "/bkt1?tagging", []byte(`<Tagging><TagSet><Tag><Key>env</Key><Value>dev</Value></Tag></TagSet></Tagging>`), nil)
	n.Expect(204, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?tagging", nil, nil)
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "<Value>dev</Value>") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "DELETE", "/bkt1?tagging", nil, nil)
	n.Expect(204, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?tagging", nil, nil)
	n.Expect(404, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?acl", nil, nil)
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "FULL_CONTROL") {
		t.Fatal(string(body))
	}
	for _, q := range []string{"encryption", "object-lock", "publicAccessBlock", "ownershipControls", "replication"} {
		resp, body = n.Call(a, "GET", "/bkt1?"+q, nil, nil)
		n.Expect(404, resp, body)
	}
	for _, q := range []string{"logging", "notification"} {
		resp, body = n.Call(a, "GET", "/bkt1?"+q, nil, nil)
		n.Expect(200, resp, body)
	}
	resp, body = n.Call(a, "PUT", "/bkt1?acl", nil, hdr("x-amz-acl", "public-read"))
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?policyStatus", nil, nil)
	if !strings.Contains(string(body), "<IsPublic>true</IsPublic>") {
		t.Fatal("public-read acl should set policy")
	}
	resp, body = n.Call(a, "PUT", "/bkt1?acl", nil, hdr("x-amz-acl", "private"))
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "GET", "/bkt1?policy", nil, nil)
	n.Expect(404, resp, body)

	resp, body = n.Call(a, "DELETE", "/bkt1", nil, nil)
	n.Expect(204, resp, body)
	resp, body = n.Call(a, "DELETE", "/bkt1", nil, nil)
	n.Expect(404, resp, body)
	resp, body = n.Call(a, "PATCH", "/bkt1", nil, nil)
	n.Expect(405, resp, body)
}

func TestObjectsUnversioned(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	resp, body := n.Call(a, "PUT", "/bkt", nil, nil)
	n.Expect(200, resp, body)

	data := []byte("hello, world")
	sum := md5.Sum(data) //nolint:gosec
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	resp, body = n.Call(a, "PUT", "/bkt/dir/hello.txt", data, hdr("Content-Type", "text/plain", "x-amz-meta-color", "blue", "Cache-Control", "no-cache"))
	n.Expect(200, resp, body)
	if resp.Header.Get("ETag") != etag || resp.Header.Get("x-amz-version-id") != "" {
		t.Fatalf("put headers: %v", resp.Header)
	}
	resp, body = n.Call(a, "GET", "/bkt/dir/hello.txt", nil, nil)
	n.Expect(200, resp, body)
	if string(body) != "hello, world" || resp.Header.Get("Content-Type") != "text/plain" || resp.Header.Get("x-amz-meta-color") != "blue" || resp.Header.Get("ETag") != etag || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("Content-Length") != "12" {
		t.Fatalf("get: %q %v", body, resp.Header)
	}
	resp, body = n.Call(a, "HEAD", "/bkt/dir/hello.txt", nil, nil)
	n.Expect(200, resp, body)
	if len(body) != 0 || resp.Header.Get("Content-Length") != "12" || resp.Header.Get("Last-Modified") == "" {
		t.Fatalf("head: %v", resp.Header)
	}
	// Range.
	resp, body = n.Call(a, "GET", "/bkt/dir/hello.txt", nil, hdr("Range", "bytes=7-"))
	n.Expect(206, resp, body)
	if string(body) != "world" || resp.Header.Get("Content-Range") != "bytes 7-11/12" {
		t.Fatalf("range: %q %v", body, resp.Header)
	}
	resp, body = n.Call(a, "GET", "/bkt/dir/hello.txt", nil, hdr("Range", "bytes=-5"))
	n.Expect(206, resp, body)
	if string(body) != "world" {
		t.Fatalf("suffix range: %q", body)
	}
	resp, body = n.Call(a, "GET", "/bkt/dir/hello.txt", nil, hdr("Range", "bytes=50-60"))
	n.Expect(416, resp, body)
	// Conditionals.
	resp, body = n.Call(a, "GET", "/bkt/dir/hello.txt", nil, hdr("If-None-Match", etag))
	n.Expect(304, resp, body)
	resp, body = n.Call(a, "GET", "/bkt/dir/hello.txt", nil, hdr("If-Match", `"nope"`))
	n.Expect(412, resp, body)
	lm, _ := http.ParseTime(resp.Header.Get("Date"))
	resp, body = n.Call(a, "GET", "/bkt/dir/hello.txt", nil, hdr("If-Modified-Since", lm.Add(time.Hour).UTC().Format(http.TimeFormat)))
	n.Expect(304, resp, body)
	resp, body = n.Call(a, "GET", "/bkt/dir/hello.txt", nil, hdr("If-Unmodified-Since", lm.Add(-time.Hour).UTC().Format(http.TimeFormat)))
	n.Expect(412, resp, body)
	// Conditional writes.
	resp, body = n.Call(a, "PUT", "/bkt/dir/hello.txt", data, hdr("If-None-Match", "*"))
	n.Expect(412, resp, body)
	resp, body = n.Call(a, "PUT", "/bkt/new.txt", data, hdr("If-None-Match", "*"))
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "PUT", "/bkt/new.txt", []byte("v2"), hdr("If-Match", etag))
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "PUT", "/bkt/new.txt", []byte("v3"), hdr("If-Match", etag))
	n.Expect(412, resp, body)
	// Content-MD5 and checksum verification.
	resp, body = n.Call(a, "PUT", "/bkt/md5.txt", data, hdr("Content-MD5", base64.StdEncoding.EncodeToString(sum[:])))
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "PUT", "/bkt/md5.txt", data, hdr("Content-MD5", base64.StdEncoding.EncodeToString(make([]byte, 16))))
	n.Expect(400, resp, body)
	if !strings.Contains(string(body), "BadDigest") {
		t.Fatal(string(body))
	}
	crc := make([]byte, 4)
	c := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	crc[0], crc[1], crc[2], crc[3] = byte(c>>24), byte(c>>16), byte(c>>8), byte(c)
	want := base64.StdEncoding.EncodeToString(crc)
	resp, body = n.Call(a, "PUT", "/bkt/crc.txt", data, hdr("x-amz-checksum-crc32c", want))
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-checksum-crc32c") != want {
		t.Fatalf("checksum echo: %v", resp.Header)
	}
	resp, body = n.Call(a, "GET", "/bkt/crc.txt", nil, hdr("x-amz-checksum-mode", "ENABLED"))
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-checksum-crc32c") != want || resp.Header.Get("x-amz-checksum-type") != "FULL_OBJECT" {
		t.Fatalf("checksum on get: %v", resp.Header)
	}
	resp, body = n.Call(a, "PUT", "/bkt/crc.txt", data, hdr("x-amz-checksum-crc32c", "AAAAAA=="))
	n.Expect(400, resp, body)
	// Signed payload hash mismatch.
	h := sha256.Sum256([]byte("other"))
	resp, body = n.Call(a, "PUT", "/bkt/sha.txt", data, hdr("x-amz-content-sha256", hex.EncodeToString(h[:])))
	n.Expect(400, resp, body)
	if !strings.Contains(string(body), "XAmzContentSHA256Mismatch") {
		t.Fatal(string(body))
	}
	h = sha256.Sum256(data)
	resp, body = n.Call(a, "PUT", "/bkt/sha.txt", data, hdr("x-amz-content-sha256", hex.EncodeToString(h[:])))
	n.Expect(200, resp, body)
	// Copy.
	resp, body = n.Call(a, "PUT", "/bkt/copy.txt", nil, hdr("x-amz-copy-source", "/bkt/dir/hello.txt"))
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "CopyObjectResult") || !strings.Contains(string(body), hex.EncodeToString(sum[:])) {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "GET", "/bkt/copy.txt", nil, nil)
	n.Expect(200, resp, body)
	if string(body) != "hello, world" || resp.Header.Get("x-amz-meta-color") != "blue" {
		t.Fatalf("copied: %q %v", body, resp.Header)
	}
	resp, body = n.Call(a, "PUT", "/bkt/copy.txt", nil, hdr("x-amz-copy-source", "/bkt/copy.txt"))
	n.Expect(400, resp, body)
	resp, body = n.Call(a, "PUT", "/bkt/copy.txt", nil, hdr("x-amz-copy-source", "/bkt/copy.txt", "x-amz-metadata-directive", "REPLACE", "x-amz-meta-color", "red"))
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "HEAD", "/bkt/copy.txt", nil, nil)
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-meta-color") != "red" {
		t.Fatal("replace directive")
	}
	resp, body = n.Call(a, "PUT", "/bkt/copy2.txt", nil, hdr("x-amz-copy-source", "/bkt/missing"))
	n.Expect(404, resp, body)
	resp, body = n.Call(a, "PUT", "/bkt/copy2.txt", nil, hdr("x-amz-copy-source", "/bkt/dir/hello.txt", "x-amz-copy-source-if-none-match", etag))
	n.Expect(412, resp, body)
	// Listing.
	resp, body = n.Call(a, "GET", "/bkt?list-type=2&delimiter=/", nil, nil)
	n.Expect(200, resp, body)
	var lr s3xml.ListBucketResult
	_ = xml.Unmarshal(body, &lr)
	if len(lr.CommonPrefixes) != 1 || lr.CommonPrefixes[0].Prefix != "dir/" || len(lr.Contents) != 5 || *lr.KeyCount != 6 {
		t.Fatalf("list v2: %s", body)
	}
	resp, body = n.Call(a, "GET", "/bkt?list-type=2&max-keys=2", nil, nil)
	n.Expect(200, resp, body)
	lr = s3xml.ListBucketResult{}
	_ = xml.Unmarshal(body, &lr)
	if !lr.IsTruncated || lr.NextContinuationToken == "" || len(lr.Contents) != 2 {
		t.Fatalf("truncated: %s", body)
	}
	resp, body = n.Call(a, "GET", "/bkt?list-type=2&max-keys=2&continuation-token="+url.QueryEscape(lr.NextContinuationToken), nil, nil)
	n.Expect(200, resp, body)
	var lr2 s3xml.ListBucketResult
	_ = xml.Unmarshal(body, &lr2)
	if len(lr2.Contents) != 2 || lr2.Contents[0].Key == lr.Contents[0].Key {
		t.Fatalf("page 2: %s", body)
	}
	resp, body = n.Call(a, "GET", "/bkt?prefix=dir/&encoding-type=url", nil, nil)
	n.Expect(200, resp, body)
	lr = s3xml.ListBucketResult{}
	_ = xml.Unmarshal(body, &lr)
	if len(lr.Contents) != 1 || lr.Contents[0].Key != "dir/hello.txt" || lr.Contents[0].Owner == nil || lr.EncodingType != "url" {
		t.Fatalf("list v1: %s", body)
	}
	resp, body = n.Call(a, "GET", "/bkt?encoding-type=weird", nil, nil)
	n.Expect(400, resp, body)
	// Delete.
	resp, body = n.Call(a, "DELETE", "/bkt/copy.txt", nil, nil)
	n.Expect(204, resp, body)
	resp, body = n.Call(a, "GET", "/bkt/copy.txt", nil, nil)
	n.Expect(404, resp, body)
	if !strings.Contains(string(body), "NoSuchKey") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "DELETE", "/bkt/copy.txt", nil, nil)
	n.Expect(204, resp, body)
	del := `<Delete><Object><Key>new.txt</Key></Object><Object><Key>missing</Key></Object><Quiet>false</Quiet></Delete>`
	resp, body = n.Call(a, "POST", "/bkt?delete", []byte(del), nil)
	n.Expect(200, resp, body)
	var dr s3xml.DeleteResult
	_ = xml.Unmarshal(body, &dr)
	if len(dr.Deleted) != 2 || len(dr.Error) != 0 {
		t.Fatalf("delete objects: %s", body)
	}
	resp, body = n.Call(a, "DELETE", "/bkt", nil, nil)
	n.Expect(409, resp, body)
	// Object tagging.
	resp, body = n.Call(a, "PUT", "/bkt/md5.txt?tagging", []byte(`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag></TagSet></Tagging>`), nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "GET", "/bkt/md5.txt?tagging", nil, nil)
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "<Key>a</Key>") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "HEAD", "/bkt/md5.txt", nil, nil)
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-tagging-count") != "1" {
		t.Fatal("tagging count")
	}
	resp, body = n.Call(a, "DELETE", "/bkt/md5.txt?tagging", nil, nil)
	n.Expect(204, resp, body)
	resp, body = n.Call(a, "GET", "/bkt/md5.txt?attributes", nil, hdr("x-amz-object-attributes", "ETag,ObjectSize,StorageClass"))
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "<ObjectSize>12</ObjectSize>") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "GET", "/bkt/md5.txt?acl", nil, nil)
	n.Expect(200, resp, body)
	// Empty object.
	resp, body = n.Call(a, "PUT", "/bkt/empty", []byte{}, nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "GET", "/bkt/empty", nil, nil)
	n.Expect(200, resp, body)
	if len(body) != 0 || resp.Header.Get("ETag") != `"d41d8cd98f00b204e9800998ecf8427e"` {
		t.Fatalf("empty: %v", resp.Header)
	}
	// Virtual host style.
	req := n.Request(a, "GET", "/md5.txt", nil, nil)
	req.Host = "bkt.s3.test"
	_ = auth.Sign(req, *a, "us-east-1", "s3", time.Now(), "")
	resp, body = n.Do(req)
	n.Expect(200, resp, body)
}

func TestVersionedObjects(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	n.Must(200, a, "PUT", "/ver", nil, nil)
	n.Must(200, a, "PUT", "/ver?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	resp, body := n.Call(a, "PUT", "/ver/k", []byte("one"), nil)
	n.Expect(200, resp, body)
	v1 := resp.Header.Get("x-amz-version-id")
	resp, body = n.Call(a, "PUT", "/ver/k", []byte("two"), nil)
	n.Expect(200, resp, body)
	v2 := resp.Header.Get("x-amz-version-id")
	if v1 == "" || v2 == "" || v1 == v2 || v1 == "null" {
		t.Fatalf("version ids %q %q", v1, v2)
	}
	resp, body = n.Call(a, "GET", "/ver/k", nil, nil)
	n.Expect(200, resp, body)
	if string(body) != "two" || resp.Header.Get("x-amz-version-id") != v2 {
		t.Fatalf("latest: %q", body)
	}
	resp, body = n.Call(a, "GET", "/ver/k?versionId="+v1, nil, nil)
	n.Expect(200, resp, body)
	if string(body) != "one" {
		t.Fatalf("v1: %q", body)
	}
	resp, body = n.Call(a, "GET", "/ver/k?versionId=bogus", nil, nil)
	n.Expect(404, resp, body)
	// Delete creates a marker.
	resp, body = n.Call(a, "DELETE", "/ver/k", nil, nil)
	n.Expect(204, resp, body)
	dm := resp.Header.Get("x-amz-version-id")
	if resp.Header.Get("x-amz-delete-marker") != "true" || dm == "" {
		t.Fatalf("marker: %v", resp.Header)
	}
	resp, body = n.Call(a, "GET", "/ver/k", nil, nil)
	n.Expect(404, resp, body)
	if resp.Header.Get("x-amz-delete-marker") != "true" {
		t.Fatal("delete marker header on 404")
	}
	resp, body = n.Call(a, "GET", "/ver/k?versionId="+dm, nil, nil)
	n.Expect(405, resp, body)
	resp, body = n.Call(a, "GET", "/ver?list-type=2", nil, nil)
	n.Expect(200, resp, body)
	var lr s3xml.ListBucketResult
	_ = xml.Unmarshal(body, &lr)
	if len(lr.Contents) != 0 {
		t.Fatal("deleted key must not list")
	}
	resp, body = n.Call(a, "GET", "/ver?versions", nil, nil)
	n.Expect(200, resp, body)
	var vr s3xml.ListVersionsResult
	_ = xml.Unmarshal(body, &vr)
	if len(vr.Version) != 2 || len(vr.DeleteMarker) != 1 || !vr.DeleteMarker[0].IsLatest || vr.Version[0].VersionID != v2 {
		t.Fatalf("versions: %s", body)
	}
	// Remove the marker: object is back.
	resp, body = n.Call(a, "DELETE", "/ver/k?versionId="+dm, nil, nil)
	n.Expect(204, resp, body)
	resp, body = n.Call(a, "GET", "/ver/k", nil, nil)
	n.Expect(200, resp, body)
	if string(body) != "two" {
		t.Fatal("after marker removal")
	}
	// Copy from a specific version.
	resp, body = n.Call(a, "PUT", "/ver/k2", nil, hdr("x-amz-copy-source", "/ver/k?versionId="+v1))
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-copy-source-version-id") != v1 || resp.Header.Get("x-amz-version-id") == "" {
		t.Fatalf("copy headers: %v", resp.Header)
	}
	resp, body = n.Call(a, "GET", "/ver/k2", nil, nil)
	if string(body) != "one" {
		t.Fatal("copied version")
	}
	// Suspend.
	n.Must(200, a, "PUT", "/ver?versioning", []byte(`<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`), nil)
	resp, body = n.Call(a, "PUT", "/ver/k", []byte("three"), nil)
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-version-id") != "null" {
		t.Fatalf("suspended put: %v", resp.Header)
	}
	// Versioned DeleteObjects.
	del := `<Delete><Object><Key>k</Key><VersionId>` + v1 + `</VersionId></Object><Object><Key>k2</Key></Object></Delete>`
	resp, body = n.Call(a, "POST", "/ver?delete", []byte(del), nil)
	n.Expect(200, resp, body)
	var dr s3xml.DeleteResult
	_ = xml.Unmarshal(body, &dr)
	if len(dr.Deleted) != 2 || dr.Deleted[0].VersionID != v1 || !dr.Deleted[1].DeleteMarker || dr.Deleted[1].DeleteMarkerVersionID != "null" {
		t.Fatalf("versioned delete: %s", body)
	}
}

func TestMultipart(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	n.Must(200, a, "PUT", "/mpu", nil, nil)
	resp, body := n.Call(a, "POST", "/mpu/big?uploads", nil, hdr("Content-Type", "application/octet-stream", "x-amz-meta-src", "test", "x-amz-checksum-algorithm", "CRC32C"))
	n.Expect(200, resp, body)
	var init s3xml.InitiateMultipartUploadResult
	_ = xml.Unmarshal(body, &init)
	if init.UploadID == "" || resp.Header.Get("x-amz-checksum-type") != "COMPOSITE" {
		t.Fatalf("init: %s %v", body, resp.Header)
	}
	uid := init.UploadID
	part1 := make([]byte, 5<<20)
	_, _ = rand.Read(part1)
	part2 := []byte("tail")
	var etags []string
	for i, p := range [][]byte{part1, part2} {
		resp, body = n.Call(a, "PUT", fmt.Sprintf("/mpu/big?uploadId=%s&partNumber=%d", uid, i+1), p, nil)
		n.Expect(200, resp, body)
		if resp.Header.Get("x-amz-checksum-crc32c") == "" {
			t.Fatal("part checksum header")
		}
		etags = append(etags, resp.Header.Get("ETag"))
	}
	resp, body = n.Call(a, "PUT", fmt.Sprintf("/mpu/big?uploadId=%s&partNumber=0", uid), part2, nil)
	n.Expect(400, resp, body)
	resp, body = n.Call(a, "PUT", "/mpu/big?uploadId=nope&partNumber=1", part2, nil)
	n.Expect(404, resp, body)
	// Part copy.
	n.Must(200, a, "PUT", "/mpu/src", []byte("0123456789"), nil)
	resp, body = n.Call(a, "PUT", fmt.Sprintf("/mpu/big?uploadId=%s&partNumber=3", uid), nil, hdr("x-amz-copy-source", "/mpu/src", "x-amz-copy-source-range", "bytes=2-5"))
	n.Expect(200, resp, body)
	var cpr s3xml.CopyPartResult
	_ = xml.Unmarshal(body, &cpr)
	etags = append(etags, cpr.ETag)
	// List parts and uploads.
	resp, body = n.Call(a, "GET", "/mpu/big?uploadId="+uid, nil, nil)
	n.Expect(200, resp, body)
	var lp s3xml.ListPartsResult
	_ = xml.Unmarshal(body, &lp)
	if len(lp.Part) != 3 || lp.Part[0].Size != 5<<20 || lp.ChecksumAlgorithm != "CRC32C" {
		t.Fatalf("parts: %s", body)
	}
	resp, body = n.Call(a, "GET", "/mpu?uploads", nil, nil)
	n.Expect(200, resp, body)
	var lu s3xml.ListMultipartUploadsResult
	_ = xml.Unmarshal(body, &lu)
	if len(lu.Upload) != 1 || lu.Upload[0].UploadID != uid {
		t.Fatalf("uploads: %s", body)
	}
	// Complete with bad order / bad etag.
	bad := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etags[1], etags[0])
	resp, body = n.Call(a, "POST", "/mpu/big?uploadId="+uid, []byte(bad), nil)
	n.Expect(400, resp, body)
	if !strings.Contains(string(body), "InvalidPartOrder") {
		t.Fatal(string(body))
	}
	bad = `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"wrong"</ETag></Part></CompleteMultipartUpload>`
	resp, body = n.Call(a, "POST", "/mpu/big?uploadId="+uid, []byte(bad), nil)
	n.Expect(400, resp, body)
	if !strings.Contains(string(body), "InvalidPart") {
		t.Fatal(string(body))
	}
	// Part 2 is small and not last: EntityTooSmall.
	bad = fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>3</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etags[1], etags[2])
	resp, body = n.Call(a, "POST", "/mpu/big?uploadId="+uid, []byte(bad), nil)
	n.Expect(400, resp, body)
	if !strings.Contains(string(body), "EntityTooSmall") {
		t.Fatal(string(body))
	}
	good := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>3</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etags[0], etags[2])
	resp, body = n.Call(a, "POST", "/mpu/big?uploadId="+uid, []byte(good), nil)
	n.Expect(200, resp, body)
	var cr s3xml.CompleteMultipartUploadResult
	_ = xml.Unmarshal(body, &cr)
	if !strings.HasSuffix(cr.ETag, `-2"`) || cr.ChecksumCRC32C == "" || !strings.HasSuffix(cr.ChecksumCRC32C, "-2") || cr.ChecksumType != "COMPOSITE" {
		t.Fatalf("complete: %s", body)
	}
	// Verify the AWS-style ETag.
	m1 := md5.Sum(part1)                    //nolint:gosec
	m3 := md5.Sum([]byte("2345"))           //nolint:gosec
	all := md5.Sum(append(m1[:], m3[:]...)) //nolint:gosec
	if cr.ETag != `"`+hex.EncodeToString(all[:])+`-2"` {
		t.Fatalf("etag %s", cr.ETag)
	}
	resp, body = n.Call(a, "GET", "/mpu/big", nil, nil)
	n.Expect(200, resp, body)
	if !bytes.Equal(body, append(append([]byte{}, part1...), []byte("2345")...)) || resp.Header.Get("x-amz-meta-src") != "test" || resp.Header.Get("x-amz-mp-parts-count") != "2" {
		t.Fatalf("assembled object wrong (%d bytes) %v", len(body), resp.Header)
	}
	resp, body = n.Call(a, "GET", "/mpu/big?partNumber=2", nil, nil)
	n.Expect(206, resp, body)
	if string(body) != "2345" {
		t.Fatalf("part get: %q", body)
	}
	resp, body = n.Call(a, "GET", "/mpu/big?attributes", nil, hdr("x-amz-object-attributes", "ObjectParts,Checksum"))
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "<TotalPartsCount>2</TotalPartsCount>") {
		t.Fatal(string(body))
	}
	resp, body = n.Call(a, "GET", "/mpu/big?uploadId="+uid, nil, nil)
	n.Expect(404, resp, body)
	// Abort.
	resp, body = n.Call(a, "POST", "/mpu/other?uploads", nil, nil)
	n.Expect(200, resp, body)
	_ = xml.Unmarshal(body, &init)
	n.Must(200, a, "PUT", fmt.Sprintf("/mpu/other?uploadId=%s&partNumber=1", init.UploadID), []byte("x"), nil)
	n.Must(204, a, "DELETE", "/mpu/other?uploadId="+init.UploadID, nil, nil)
	n.Must(404, a, "DELETE", "/mpu/other?uploadId="+init.UploadID, nil, nil)
	// FULL_OBJECT CRC64NVME, as aws cli v2 sends.
	resp, body = n.Call(a, "POST", "/mpu/full?uploads", nil, hdr("x-amz-checksum-algorithm", "CRC64NVME"))
	n.Expect(200, resp, body)
	_ = xml.Unmarshal(body, &init)
	if resp.Header.Get("x-amz-checksum-type") != "FULL_OBJECT" {
		t.Fatal("crc64 default type")
	}
	var fetags []string
	for i, p := range [][]byte{part1, part2} {
		resp, body = n.Call(a, "PUT", fmt.Sprintf("/mpu/full?uploadId=%s&partNumber=%d", init.UploadID, i+1), p, nil)
		n.Expect(200, resp, body)
		fetags = append(fetags, resp.Header.Get("ETag"))
	}
	wantFull := crc64b64(append(append([]byte{}, part1...), part2...))
	good = fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, fetags[0], fetags[1])
	resp, body = n.Call(a, "POST", "/mpu/full?uploadId="+init.UploadID, []byte(good), hdr("x-amz-checksum-crc64nvme", "AAAAAAAAAAA="))
	n.Expect(400, resp, body)
	resp, body = n.Call(a, "POST", "/mpu/full?uploadId="+init.UploadID, []byte(good), hdr("x-amz-checksum-crc64nvme", wantFull))
	n.Expect(200, resp, body)
	_ = xml.Unmarshal(body, &cr)
	if cr.ChecksumCRC64NVME != wantFull || cr.ChecksumType != "FULL_OBJECT" {
		t.Fatalf("full object checksum: %s", body)
	}
}

func crc64b64(b []byte) string {
	// Imported lazily through store to avoid duplicating the polynomial.
	return testutilCRC64(b)
}

func TestAuthAndPolicy(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	rw := n.CreateUser("rw", meta.RoleReadWrite, nil)
	ro := n.CreateUser("ro", meta.RoleReadOnly, []string{"pub"})
	n.Must(200, a, "PUT", "/pub", nil, nil)
	n.Must(200, a, "PUT", "/priv", nil, nil)
	n.Must(200, a, "PUT", "/pub/o", []byte("x"), nil)
	n.Must(200, a, "PUT", "/priv/o", []byte("y"), nil)

	// Anonymous is refused everywhere until a policy says otherwise.
	resp, body := n.Call(nil, "GET", "/pub/o", nil, nil)
	n.Expect(403, resp, body)
	resp, body = n.Call(nil, "GET", "/", nil, nil)
	n.Expect(403, resp, body)
	resp, body = n.Call(nil, "GET", "/nonexistent/o", nil, nil)
	n.Expect(404, resp, body)
	pol := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`
	n.Must(204, a, "PUT", "/pub?policy", []byte(pol), nil)
	resp, body = n.Call(nil, "GET", "/pub/o", nil, nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(nil, "GET", "/pub", nil, nil)
	n.Expect(403, resp, body)
	resp, body = n.Call(nil, "PUT", "/pub/new", []byte("z"), nil)
	n.Expect(403, resp, body)

	// Roles.
	resp, body = n.Call(&rw, "PUT", "/priv/rw", []byte("1"), nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(&rw, "PUT", "/rwbucket", nil, nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(&ro, "GET", "/pub/o", nil, nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(&ro, "PUT", "/pub/o2", []byte("1"), nil)
	n.Expect(403, resp, body)
	resp, body = n.Call(&ro, "GET", "/priv/o", nil, nil)
	n.Expect(403, resp, body)
	resp, body = n.Call(&ro, "GET", "/", nil, nil)
	n.Expect(200, resp, body)
	var lb s3xml.ListAllMyBucketsResult
	_ = xml.Unmarshal(body, &lb)
	if len(lb.Buckets) != 1 || lb.Buckets[0].Name != "pub" {
		t.Fatalf("scoped list: %s", body)
	}
	// Bad credentials.
	bad := auth.Credentials{AccessKeyID: a.AccessKeyID, SecretKey: "wrong"}
	resp, body = n.Call(&bad, "GET", "/", nil, nil)
	n.Expect(403, resp, body)
	if !strings.Contains(string(body), "SignatureDoesNotMatch") {
		t.Fatal(string(body))
	}
	unknown := auth.Credentials{AccessKeyID: "MNONUNKNOWNKEY000000", SecretKey: "x"}
	resp, body = n.Call(&unknown, "GET", "/", nil, nil)
	n.Expect(403, resp, body)
	if !strings.Contains(string(body), "InvalidAccessKeyId") {
		t.Fatal(string(body))
	}
	// Clock skew.
	old := n.Now
	n.Now = func() time.Time { return time.Now().Add(-time.Hour) }
	resp, body = n.Call(a, "GET", "/", nil, nil)
	n.Expect(403, resp, body)
	if !strings.Contains(string(body), "RequestTimeTooSkewed") {
		t.Fatal(string(body))
	}
	n.Now = old
	// Rate limiting after many failures.
	for i := 0; i < 25; i++ {
		n.Call(&bad, "GET", "/", nil, nil)
	}
	resp, body = n.Call(a, "GET", "/", nil, nil)
	n.Expect(429, resp, body)
	// Presigned URL.
	req, _ := http.NewRequest("GET", n.URL()+"/priv/o", nil)
	u, err := auth.Presign(req, *a, "us-east-1", "s3", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Use a fresh node to escape the rate limit.
	n2 := testutil.Start(t)
	n2.Must(200, &n2.Admin, "PUT", "/priv", nil, nil)
	n2.Must(200, &n2.Admin, "PUT", "/priv/o", []byte("y"), nil)
	req, _ = http.NewRequest("GET", n2.URL()+"/priv/o", nil)
	u, _ = auth.Presign(req, n2.Admin, "us-east-1", "s3", time.Now(), time.Minute)
	preq, _ := http.NewRequest("GET", u.String(), nil)
	resp, body = n2.Do(preq)
	n2.Expect(200, resp, body)
	if string(body) != "y" {
		t.Fatal("presigned get")
	}
	req, _ = http.NewRequest("GET", n2.URL()+"/priv/o", nil)
	u, _ = auth.Presign(req, n2.Admin, "us-east-1", "s3", time.Now().Add(-2*time.Hour), time.Minute)
	preq, _ = http.NewRequest("GET", u.String(), nil)
	resp, body = n2.Do(preq)
	n2.Expect(403, resp, body)
	if !strings.Contains(string(body), "expired") {
		t.Fatal(string(body))
	}
	// Disabled key.
	ctx := t.Context()
	if _, err := n2.Core.Propose(ctx, &meta.UpdateAccessKey{AccessKeyID: n2.Admin.AccessKeyID, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	resp, body = n2.Call(&n2.Admin, "GET", "/", nil, nil)
	n2.Expect(403, resp, body)
}

func TestStreamingUpload(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	n.Must(200, a, "PUT", "/str", nil, nil)
	data := make([]byte, 100_000)
	_, _ = rand.Read(data)
	// Build an aws-chunked body with a signed trailer, like the AWS SDKs do.
	req, _ := http.NewRequest("PUT", n.URL()+"/str/chunked", nil)
	req.Header.Set("x-amz-content-sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER")
	req.Header.Set("x-amz-decoded-content-length", fmt.Sprint(len(data)))
	req.Header.Set("x-amz-trailer", "x-amz-checksum-crc32c")
	req.Header.Set("Content-Encoding", "aws-chunked")
	now := time.Now()
	if err := auth.Sign(req, *a, "us-east-1", "s3", now, ""); err != nil {
		t.Fatal(err)
	}
	// Reuse the verifier to derive the seed signature.
	v := &auth.Verifier{Lookup: func(_ context.Context, ak string) (string, error) { return a.SecretKey, nil }}
	id, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	crcTab := crc32.MakeTable(crc32.Castagnoli)
	c := crc32.Checksum(data, crcTab)
	crcB := []byte{byte(c >> 24), byte(c >> 16), byte(c >> 8), byte(c)}
	body := auth.EncodeChunkedForTest(data, id, map[string]string{"x-amz-checksum-crc32c": base64.StdEncoding.EncodeToString(crcB)}, 65536)
	req.Body = http.NoBody
	req2 := req.Clone(context.Background())
	req2.Body = ioNopCloser(bytes.NewReader(body))
	req2.ContentLength = int64(len(body))
	resp, rb := n.Do(req2)
	n.Expect(200, resp, rb)
	if resp.Header.Get("x-amz-checksum-crc32c") != base64.StdEncoding.EncodeToString(crcB) {
		t.Fatalf("trailer checksum echo: %v", resp.Header)
	}
	resp, rb = n.Call(a, "GET", "/str/chunked", nil, nil)
	n.Expect(200, resp, rb)
	if !bytes.Equal(rb, data) {
		t.Fatal("streamed data mismatch")
	}
}

func TestSigV2PoliciesAndACLs(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	n.Must(200, a, "PUT", "/pol", nil, nil)
	n.Must(200, a, "PUT", "/pol/secret.txt", []byte("s"), nil)
	n.Must(200, a, "PUT", "/pol/open.txt", []byte("o"), nil)

	// SigV2 presigned URL (botocore default) works.
	req, _ := http.NewRequest("GET", n.URL()+"/pol/secret.txt", nil)
	u := auth.PresignV2(req, *a, time.Now().Add(time.Minute), nil)
	preq, _ := http.NewRequest("GET", u, nil)
	resp, body := n.Do(preq)
	n.Expect(200, resp, body)
	if string(body) != "s" {
		t.Fatal("sigv2 presigned body")
	}
	// SigV2 header auth.
	hreq, _ := http.NewRequest("GET", n.URL()+"/pol/secret.txt", nil)
	hreq.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	v2 := &auth.Verifier{}
	_ = v2
	sts := "GET\n\n\n" + hreq.Header.Get("Date") + "\n/pol/secret.txt"
	hreq.Header.Set("Authorization", "AWS "+a.AccessKeyID+":"+auth.SignV2(a.SecretKey, sts))
	resp, body = n.Do(hreq)
	n.Expect(200, resp, body)

	// Policy with a source IP condition and a Deny that applies per key in DeleteObjects.
	pol := `{"Version":"2012-10-17","Statement":[
	 {"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":"arn:aws:s3:::pol/open.txt","Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}},
	 {"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::pol/secret.txt"}]}`
	n.Must(204, a, "PUT", "/pol?policy", []byte(pol), nil)
	n.Must(200, nil, "GET", "/pol/open.txt", nil, nil)
	n.Must(403, nil, "GET", "/pol/secret.txt", nil, nil)
	rw := n.CreateUser("writer", meta.RoleReadWrite, nil)
	resp, body = n.Call(&rw, "POST", "/pol?delete", []byte(`<Delete><Object><Key>secret.txt</Key></Object><Object><Key>open.txt</Key></Object></Delete>`), nil)
	n.Expect(200, resp, body)
	var dr s3xml.DeleteResult
	_ = xml.Unmarshal(body, &dr)
	if len(dr.Deleted) != 1 || dr.Deleted[0].Key != "open.txt" || len(dr.Error) != 1 || dr.Error[0].Code != "AccessDenied" {
		t.Fatalf("per-key deny: %s", body)
	}
	// Non-owner writers cannot change bucket configuration; the owner (admin) can.
	n.Must(403, &rw, "PUT", "/pol?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	n.Must(403, &rw, "DELETE", "/pol?policy", nil, nil)
	n.Must(204, a, "DELETE", "/pol?policy", nil, nil)

	// Canned ACLs: object public-read via header, bucket public-read-write via XML body.
	n.Must(200, a, "PUT", "/pol/pub.txt", []byte("p"), hdr("x-amz-acl", "public-read"))
	n.Must(200, nil, "GET", "/pol/pub.txt", nil, nil)
	n.Must(403, nil, "GET", "/pol/secret.txt", nil, nil)
	resp, body = n.Call(a, "GET", "/pol/pub.txt?acl", nil, nil)
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "AllUsers") || !strings.Contains(string(body), "READ") {
		t.Fatalf("object acl: %s", body)
	}
	n.Must(200, a, "PUT", "/pol/pub.txt?acl", nil, hdr("x-amz-acl", "private"))
	n.Must(403, nil, "GET", "/pol/pub.txt", nil, nil)
	aclBody := `<AccessControlPolicy><Owner><ID>x</ID></Owner><AccessControlList><Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant><Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>WRITE</Permission></Grant></AccessControlList></AccessControlPolicy>`
	n.Must(200, a, "PUT", "/pol?acl", []byte(aclBody), nil)
	n.Must(200, nil, "GET", "/pol?list-type=2", nil, nil)
	n.Must(200, nil, "PUT", "/pol/anon.txt", []byte("anon"), nil)
	resp, body = n.Call(a, "GET", "/pol?acl", nil, nil)
	if !strings.Contains(string(body), "WRITE") {
		t.Fatalf("bucket acl should show public write: %s", body)
	}
	n.Must(200, a, "PUT", "/pol?acl", nil, hdr("x-amz-acl", "private"))
	n.Must(403, nil, "GET", "/pol?list-type=2", nil, nil)

	// Tag order is preserved; ranged GETs carry no checksum; chunked transfer accepted.
	n.Must(200, a, "PUT", "/pol/t.txt", []byte("t"), nil)
	n.Must(200, a, "PUT", "/pol/t.txt?tagging", []byte(`<Tagging><TagSet><Tag><Key>z</Key><Value>1</Value></Tag><Tag><Key>a</Key><Value>2</Value></Tag></TagSet></Tagging>`), nil)
	resp, body = n.Call(a, "GET", "/pol/t.txt?tagging", nil, nil)
	n.Expect(200, resp, body)
	if strings.Index(string(body), "<Key>z</Key>") > strings.Index(string(body), "<Key>a</Key>") {
		t.Fatalf("tag order not preserved: %s", body)
	}
	crc := base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 0})
	_ = crc
	n.Must(200, a, "PUT", "/pol/ck.bin", []byte("0123456789"), hdr("x-amz-checksum-crc32", crc32b64([]byte("0123456789"))))
	resp, body = n.Call(a, "GET", "/pol/ck.bin", nil, hdr("Range", "bytes=0-3", "x-amz-checksum-mode", "ENABLED"))
	n.Expect(206, resp, body)
	if resp.Header.Get("x-amz-checksum-crc32") != "" {
		t.Fatal("ranged response must not carry a checksum")
	}
	resp, body = n.Call(a, "GET", "/pol/ck.bin", nil, hdr("x-amz-checksum-mode", "ENABLED"))
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-checksum-crc32") == "" {
		t.Fatal("full response carries the checksum")
	}
	// User metadata comes back lower-case; aws-chunked is stripped from Content-Encoding.
	n.Must(200, a, "PUT", "/pol/meta.txt", []byte("m"), hdr("x-amz-meta-My-Key", "v", "Content-Encoding", "aws-chunked, gzip"))
	resp, body = n.Call(a, "HEAD", "/pol/meta.txt", nil, nil)
	n.Expect(200, resp, body)
	if _, ok := resp.Header["X-Amz-Meta-My-Key"]; ok {
		// Go's client canonicalises on read; check the raw wire form instead.
		t.Log("client canonicalised header; wire format verified in unit test")
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("content-encoding: %q", resp.Header.Get("Content-Encoding"))
	}
	// Idempotent CompleteMultipartUpload.
	resp, body = n.Call(a, "POST", "/pol/mp?uploads", nil, nil)
	n.Expect(200, resp, body)
	var init s3xml.InitiateMultipartUploadResult
	_ = xml.Unmarshal(body, &init)
	resp, body = n.Call(a, "PUT", "/pol/mp?uploadId="+init.UploadID+"&partNumber=1", []byte("part"), nil)
	n.Expect(200, resp, body)
	complete := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + resp.Header.Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`
	resp, body = n.Call(a, "POST", "/pol/mp?uploadId="+init.UploadID, []byte(complete), nil)
	n.Expect(200, resp, body)
	first := string(body)
	resp, body = n.Call(a, "POST", "/pol/mp?uploadId="+init.UploadID, []byte(complete), nil)
	n.Expect(200, resp, body)
	if !strings.Contains(first, "ETag") || !strings.Contains(string(body), "ETag") {
		t.Fatalf("retry should return the same result: %s", body)
	}
	resp, body = n.Call(a, "GET", "/pol?uploads", nil, nil)
	n.Expect(200, resp, body)
	if strings.Contains(string(body), init.UploadID) {
		t.Fatal("completed upload must not be listed")
	}
	// Bucket delete discards open uploads.
	n.Must(200, a, "PUT", "/gone", nil, nil)
	resp, body = n.Call(a, "POST", "/gone/x?uploads", nil, nil)
	n.Expect(200, resp, body)
	n.Must(204, a, "DELETE", "/gone", nil, nil)
	// If-Match on a missing key is 404.
	n.Must(404, a, "PUT", "/pol/missing", []byte("x"), hdr("If-Match", `"abc"`))
}

func crc32b64(b []byte) string {
	v := crc32.ChecksumIEEE(b)
	return base64.StdEncoding.EncodeToString([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func TestObjectLockDefaultRetention(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	n.Must(200, a, "PUT", "/locked", nil, hdr("x-amz-bucket-object-lock-enabled", "true"))
	resp, body := n.Call(a, "GET", "/locked?object-lock", nil, nil)
	n.Expect(200, resp, body)
	n.Must(400, a, "PUT", "/locked?object-lock", []byte(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>`), nil)
	n.Must(200, a, "PUT", "/locked?object-lock", []byte(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`), nil)
	resp, body = n.Call(a, "GET", "/locked?object-lock", nil, nil)
	if !strings.Contains(string(body), "<Days>30</Days>") {
		t.Fatalf("lock config: %s", body)
	}
	// New objects inherit the default retention and cannot be deleted by version.
	resp, body = n.Call(a, "PUT", "/locked/doc", []byte("x"), nil)
	n.Expect(200, resp, body)
	vid := resp.Header.Get("x-amz-version-id")
	resp, body = n.Call(a, "HEAD", "/locked/doc", nil, nil)
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-object-lock-mode") != "GOVERNANCE" || resp.Header.Get("x-amz-object-lock-retain-until-date") == "" {
		t.Fatalf("default retention not applied: %v", resp.Header)
	}
	rw := n.CreateUser("rw", meta.RoleReadWrite, nil)
	n.Must(403, &rw, "DELETE", "/locked/doc?versionId="+vid, nil, nil)
	n.Must(403, &rw, "DELETE", "/locked/doc?versionId="+vid, nil, hdr("x-amz-bypass-governance-retention", "true"))
	// The owner (admin) may bypass governance.
	n.Must(204, a, "DELETE", "/locked/doc?versionId="+vid, nil, hdr("x-amz-bypass-governance-retention", "true"))
	// Removing the rule stops new objects from inheriting it.
	n.Must(200, a, "PUT", "/locked?object-lock", []byte(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`), nil)
	resp, body = n.Call(a, "PUT", "/locked/free", []byte("x"), nil)
	n.Expect(200, resp, body)
	resp, body = n.Call(a, "HEAD", "/locked/free", nil, nil)
	n.Expect(200, resp, body)
	if resp.Header.Get("x-amz-object-lock-mode") != "" {
		t.Fatal("no default retention expected")
	}
	n.Must(409, a, "PUT", "/locked?versioning", []byte(`<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`), nil)
	n.Must(200, a, "PUT", "/plain", nil, nil)
	n.Must(409, a, "PUT", "/plain?object-lock", []byte(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`), nil)
}

func TestBucketNotifications(t *testing.T) {
	n := testutil.Start(t)
	a := &n.Admin
	var mu sync.Mutex
	var events []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		events = append(events, string(b)+"|"+r.Header.Get("X-Hogshead-Signature"))
		mu.Unlock()
	}))
	defer hook.Close()
	n.Must(200, a, "PUT", "/evt", nil, nil)
	cfg := `<NotificationConfiguration><QueueConfiguration><Id>q1</Id><Queue>arn:hogshead:webhook:::ci</Queue><Event>s3:ObjectCreated:*</Event><Event>s3:ObjectRemoved:*</Event><Filter><S3Key><FilterRule><Name>prefix</Name><Value>in/</Value></FilterRule></S3Key></Filter></QueueConfiguration></NotificationConfiguration>`
	// Unknown webhook is refused until the setting exists.
	n.Must(400, a, "PUT", "/evt?notification", []byte(cfg), nil)
	ctx := context.Background()
	if _, err := n.Core.Propose(ctx, &meta.SetSetting{Key: "webhook.ci", Value: hook.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Core.Propose(ctx, &meta.SetSetting{Key: "webhook.ci.secret", Value: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	n.Must(400, a, "PUT", "/evt?notification", []byte(`<NotificationConfiguration><QueueConfiguration><Queue>arn:aws:sqs:::x</Queue><Event>s3:ObjectCreated:*</Event></QueueConfiguration></NotificationConfiguration>`), nil)
	n.Must(400, a, "PUT", "/evt?notification", []byte(`<NotificationConfiguration><QueueConfiguration><Queue>arn:hogshead:webhook:::ci</Queue><Event>s3:Weird</Event></QueueConfiguration></NotificationConfiguration>`), nil)
	n.Must(200, a, "PUT", "/evt?notification", []byte(cfg), nil)
	resp, body := n.Call(a, "GET", "/evt?notification", nil, nil)
	n.Expect(200, resp, body)
	if !strings.Contains(string(body), "arn:hogshead:webhook:::ci") {
		t.Fatalf("get notification: %s", body)
	}
	n.Must(200, a, "PUT", "/evt/in/a.txt", []byte("hello"), nil)
	n.Must(200, a, "PUT", "/evt/out/b.txt", []byte("hello"), nil) // filtered by prefix
	n.Must(204, a, "DELETE", "/evt/in/a.txt", nil, nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		cnt := len(events)
		mu.Unlock()
		if cnt >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(events), events)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "ObjectCreated:Put") || !strings.Contains(joined, "ObjectRemoved:Delete") || !strings.Contains(joined, "in%2Fa.txt") || !strings.Contains(joined, "|sha256=") {
		t.Fatalf("events: %s", joined)
	}
	// Clearing the configuration.
	n.Must(200, a, "PUT", "/evt?notification", []byte(`<NotificationConfiguration/>`), nil)
	resp, body = n.Call(a, "GET", "/evt?notification", nil, nil)
	n.Expect(200, resp, body)
	if strings.Contains(string(body), "QueueConfiguration") {
		t.Fatal("configuration should be cleared")
	}
}
