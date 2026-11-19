package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSigV2(t *testing.T) {
	// AWS documentation example (Signing and Authenticating REST Requests).
	secret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	sts := "GET\n\n\nTue, 27 Mar 2007 19:36:42 +0000\n/awsexamplebucket1/photos/puppy.jpg"
	if got := SignV2(secret, sts); got != "qgk2+6Sv9/oM7G3qLEjTH1a1l1g=" {
		t.Fatalf("doc example signature: %s", got)
	}
	now := time.Date(2007, 3, 27, 19, 36, 42, 0, time.UTC)
	v := &Verifier{
		Lookup:              func(_ context.Context, ak string) (string, error) { return secret, nil },
		Now:                 func() time.Time { return now },
		VirtualHostSuffixes: []string{"s3.amazonaws.com"},
		AllowV2:             true,
	}
	// Header form, virtual-host style: the bucket comes from the Host header.
	r := httptest.NewRequest("GET", "http://awsexamplebucket1.s3.amazonaws.com/photos/puppy.jpg", nil)
	r.Host = "awsexamplebucket1.s3.amazonaws.com"
	r.Header.Set("Date", "Tue, 27 Mar 2007 19:36:42 +0000")
	r.Header.Set("Authorization", "AWS AKIAIOSFODNN7EXAMPLE:qgk2+6Sv9/oM7G3qLEjTH1a1l1g=")
	id, err := v.Verify(context.Background(), r)
	if err != nil || id.Version != 2 || id.AccessKeyID != "AKIAIOSFODNN7EXAMPLE" || id.Presigned {
		t.Fatalf("v2 header: %+v %v", id, err)
	}
	r.Header.Set("Authorization", "AWS AKIAIOSFODNN7EXAMPLE:wrong=")
	if _, err := v.Verify(context.Background(), r); err != ErrSignatureMismatch {
		t.Fatalf("want mismatch got %v", err)
	}
	// Skew.
	v.Now = func() time.Time { return now.Add(time.Hour) }
	r.Header.Set("Authorization", "AWS AKIAIOSFODNN7EXAMPLE:qgk2+6Sv9/oM7G3qLEjTH1a1l1g=")
	if _, err := v.Verify(context.Background(), r); err != ErrSkew {
		t.Fatalf("want skew got %v", err)
	}
	v.Now = func() time.Time { return now }
	// Path style with sub-resources and x-amz headers, round trip through our own signer.
	r2 := httptest.NewRequest("PUT", "http://127.0.0.1:9000/b/k%20ey?acl&foo=bar&versionId=v1", nil)
	r2.Host = "127.0.0.1:9000"
	r2.Header.Set("x-amz-date", "Tue, 27 Mar 2007 19:36:42 +0000")
	r2.Header.Set("x-amz-meta-b", "2")
	r2.Header.Set("x-amz-meta-a", " 1 ")
	r2.Header.Set("Content-Type", "text/plain")
	r2.Header.Set("Content-MD5", "md5")
	want := "PUT\nmd5\ntext/plain\n\nx-amz-date:Tue, 27 Mar 2007 19:36:42 +0000\nx-amz-meta-a:1\nx-amz-meta-b:2\n/b/k%20ey?acl&versionId=v1"
	if got := v.v2StringToSign(r2, "", false, ""); got != want {
		t.Fatalf("string to sign:\n%q\nwant\n%q", got, want)
	}
	r2.Header.Set("Authorization", "AWS AK:"+SignV2(secret, want))
	if _, err := v.Verify(context.Background(), r2); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	// Query form.
	r3 := httptest.NewRequest("GET", "http://127.0.0.1:9000/b/k", nil)
	r3.Host = "127.0.0.1:9000"
	u := PresignV2(r3, Credentials{AccessKeyID: "AK", SecretKey: secret}, now.Add(time.Hour), nil)
	r4 := httptest.NewRequest("GET", u, nil)
	r4.Host = "127.0.0.1:9000"
	id, err = v.Verify(context.Background(), r4)
	if err != nil || !id.Presigned || id.Version != 2 || id.Expires.IsZero() {
		t.Fatalf("v2 query: %+v %v", id, err)
	}
	expired := PresignV2(r3, Credentials{AccessKeyID: "AK", SecretKey: secret}, now.Add(-time.Hour), nil)
	r5 := httptest.NewRequest("GET", expired, nil)
	if _, err := v.Verify(context.Background(), r5); err != ErrExpired {
		t.Fatalf("want expired got %v", err)
	}
	// Disabled V2.
	v.AllowV2 = false
	if _, err := v.Verify(context.Background(), r4); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("v2 disabled: %v", err)
	}
	// Malformed.
	v.AllowV2 = true
	bad := httptest.NewRequest("GET", "http://h/b/k", nil)
	bad.Header.Set("Authorization", "AWS nocolon")
	if _, err := v.Verify(context.Background(), bad); err == nil {
		t.Fatal("malformed v2")
	}
	bad.Header.Set("Authorization", "AWS a:b")
	if _, err := v.Verify(context.Background(), bad); err == nil {
		t.Fatal("missing date")
	}
	bad.Header.Set("Date", "garbage")
	if _, err := v.Verify(context.Background(), bad); err == nil {
		t.Fatal("bad date")
	}
	if _, err := parseV2Date("20130524T000000Z"); err != nil {
		t.Fatal("amz date format accepted")
	}
	q := httptest.NewRequest("GET", "http://h/b/k?AWSAccessKeyId=a&Signature=s&Expires=notanumber", nil)
	if _, err := v.Verify(context.Background(), q); err == nil {
		t.Fatal("bad expires")
	}
	q = httptest.NewRequest("GET", "http://h/b/k?AWSAccessKeyId=a&Signature=s", nil)
	if _, err := v.Verify(context.Background(), q); err == nil {
		t.Fatal("missing expires")
	}
	_ = http.MethodGet
}

func TestRequireSignedHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "http://h/b/k", nil)
	r.Header.Set("x-amz-meta-x", "1")
	r.Header.Set("x-amz-date", "d")
	r.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	if err := requireSignedHeaders(r, []string{"host", "x-amz-meta-x"}); err != nil {
		t.Fatal(err)
	}
	if err := requireSignedHeaders(r, []string{"x-amz-meta-x"}); err == nil {
		t.Fatal("host must be required")
	}
	if err := requireSignedHeaders(r, []string{"host"}); err == nil {
		t.Fatal("unsigned x-amz header must be rejected")
	}
}
