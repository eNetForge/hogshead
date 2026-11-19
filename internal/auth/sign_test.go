package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var testCreds = Credentials{AccessKeyID: suiteAccessKey, SecretKey: suiteSecret}

func TestSignThenVerifyPaths(t *testing.T) {
	targets := []string{
		"http://localhost:9000/",
		"http://localhost:9000/bucket",
		"http://localhost:9000/bucket/",
		"http://localhost:9000/bucket/my key with spaces.txt",
		"http://localhost:9000/bucket/my%20key%20with%20spaces.txt",
		"http://localhost:9000/bucket/ünïcödé/日本語.txt",
		"http://localhost:9000/bucket/a+b.txt",
		"http://localhost:9000/bucket/a%2Bb.txt",
		"http://localhost:9000/bucket/key?list-type=2&prefix=a%2Fb&delimiter=/",
		"http://localhost:9000/bucket/key?a=1+2&b=x%3Dy&c=p%2Bq",
		"http://localhost:9000/bucket/key?k=2&k=1&k=10",
		"http://localhost:9000/bucket?uploads",
		"http://localhost:9000/bucket/key?versionId=abc&partNumber=1&uploadId=x%2Fy",
		"http://localhost:9000/bucket/weird*chars()!'.txt",
	}
	v := suiteVerifier(suiteTime)
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			// http.NewRequest (unlike httptest) accepts raw spaces and unicode
			// in the target, as a client URL would.
			r, err := http.NewRequest("PUT", target, strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Content-Type", "text/plain")
			r.Header.Set("X-Amz-Meta-Foo", "  bar   baz ")
			hash := sha256Hex([]byte("body"))
			if err := Sign(r, testCreds, suiteRegion, "s3", suiteTime, hash); err != nil {
				t.Fatal(err)
			}
			if r.Header.Get(headerContentSHA256) != hash {
				t.Error("Sign should add x-amz-content-sha256")
			}
			id := mustVerify(t, v, r)
			if id.Payload != PayloadSigned || id.Service != "s3" {
				t.Errorf("identity = %+v", id)
			}
			// Simulate the wire: the server sees the raw request URI.
			wire := httptest.NewRequest("PUT", r.URL.RequestURI(), nil)
			wire.Host = r.Host
			wire.Header = r.Header.Clone()
			mustVerify(t, v, wire)
		})
	}
}

func TestSignKeepsExistingContentSHA256(t *testing.T) {
	r := httptest.NewRequest("PUT", "http://h/b/k", nil)
	r.Header.Set(headerContentSHA256, unsignedPayload)
	if err := Sign(r, testCreds, suiteRegion, "s3", suiteTime, emptySHA256); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get(headerContentSHA256) != unsignedPayload {
		t.Error("existing header must be preserved")
	}
	id := mustVerify(t, suiteVerifier(suiteTime), r)
	if id.Payload != PayloadUnsigned {
		t.Errorf("payload = %v", id.Payload)
	}
}

func TestSignExcludesHopByHopHeaders(t *testing.T) {
	r := httptest.NewRequest("PUT", "http://h/b/k", nil)
	r.Header.Set("Connection", "keep-alive")
	r.Header.Set("Content-Length", "4")
	r.Header.Set("Authorization", "stale")
	if err := Sign(r, testCreds, suiteRegion, "s3", suiteTime, ""); err != nil {
		t.Fatal(err)
	}
	authz := r.Header.Get(headerAuthorization)
	for _, h := range []string{"connection", "content-length", "authorization"} {
		if strings.Contains(authz, h) {
			t.Errorf("%s must not be signed: %s", h, authz)
		}
	}
	if !strings.Contains(authz, "SignedHeaders=host;x-amz-date,") {
		t.Errorf("unexpected signed headers: %s", authz)
	}
}

func TestSignNilHeaderAndValidation(t *testing.T) {
	r := &http.Request{Method: "GET", URL: &url.URL{Scheme: "http", Host: "h", Path: "/"}}
	if err := Sign(r, testCreds, suiteRegion, "s3", suiteTime, ""); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get(headerAuthorization) == "" {
		t.Error("Authorization not set")
	}
	bad := []struct {
		name string
		req  *http.Request
		c    Credentials
		reg  string
		svc  string
	}{
		{"nil request", nil, testCreds, suiteRegion, "s3"},
		{"nil url", &http.Request{}, testCreds, suiteRegion, "s3"},
		{"empty creds", httptest.NewRequest("GET", "http://h/", nil), Credentials{}, suiteRegion, "s3"},
		{"empty region", httptest.NewRequest("GET", "http://h/", nil), testCreds, "", "s3"},
		{"empty service", httptest.NewRequest("GET", "http://h/", nil), testCreds, suiteRegion, ""},
	}
	for _, b := range bad {
		if err := Sign(b.req, b.c, b.reg, b.svc, suiteTime, ""); err == nil {
			t.Errorf("%s: expected error", b.name)
		}
		if _, err := Presign(b.req, b.c, b.reg, b.svc, suiteTime, time.Hour); err == nil {
			t.Errorf("%s: expected presign error", b.name)
		}
	}
}

func presignedRequest(t *testing.T, target string, expires time.Duration) *http.Request {
	t.Helper()
	r, err := http.NewRequest("GET", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := Presign(r, testCreds, suiteRegion, "s3", suiteTime, expires)
	if err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRequest("GET", u.String(), nil)
	return out
}

func TestPresignThenVerify(t *testing.T) {
	targets := []string{
		"http://localhost:9000/bucket/key.txt",
		"http://localhost:9000/bucket/my key/ünï.txt?response-content-type=text%2Fplain&a=1+2",
		"http://localhost:9000/bucket/a+b.txt?k=2&k=1",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			r := presignedRequest(t, target, time.Hour)
			q := r.URL.Query()
			if q.Get(queryAlgorithm) != algorithm || q.Get(querySignedHeaders) != "host" || q.Get(queryExpires) != "3600" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			if q.Get(queryCredential) != suiteAccessKey+"/20150830/us-east-1/s3/aws4_request" {
				t.Errorf("credential = %s", q.Get(queryCredential))
			}
			v := suiteVerifier(suiteTime.Add(59 * time.Minute))
			id := mustVerify(t, v, r)
			if !id.Presigned || id.Payload != PayloadUnsigned || !id.Expires.Equal(suiteTime.Add(time.Hour)) {
				t.Errorf("identity = %+v", id)
			}
			if !id.Date.Equal(suiteTime) || id.Service != "s3" || len(id.SignedHeaders) != 1 {
				t.Errorf("identity = %+v", id)
			}
		})
	}
}

func TestPresignPreservesOriginalQueryAndStripsOldAuth(t *testing.T) {
	r := httptest.NewRequest("GET", "http://h/b/k?x=1&X-Amz-Signature=old&X-Amz-Expires=5", nil)
	u, err := Presign(r, testCreds, suiteRegion, "s3", suiteTime, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pairs := parseRawQuery(u.RawQuery)
	count := func(name string) int {
		n := 0
		for _, p := range pairs {
			if p.key == name {
				n++
			}
		}
		return n
	}
	if count("x") != 1 || count(querySignature) != 1 || count(queryExpires) != 1 {
		t.Errorf("query = %s", u.RawQuery)
	}
	if v, _ := queryLookup(pairs, queryExpires); v != "30" {
		t.Errorf("expires = %s", v)
	}
	if r.URL.RawQuery != "x=1&X-Amz-Signature=old&X-Amz-Expires=5" {
		t.Error("Presign must not modify the input request")
	}
}

func TestPresignUsesRequestHost(t *testing.T) {
	r := httptest.NewRequest("GET", "/b/k", nil)
	r.Host = "bucket.example.com"
	u, err := Presign(r, testCreds, suiteRegion, "s3", suiteTime, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	in := httptest.NewRequest("GET", u.RequestURI(), nil)
	in.Host = "bucket.example.com"
	mustVerify(t, suiteVerifier(suiteTime), in)
	in.Host = "other.example.com"
	_, err = suiteVerifier(suiteTime).Verify(context.Background(), in)
	wantErr(t, err, ErrSignatureMismatch)

	r.Host = ""
	r.URL.Host = ""
	if _, err := Presign(r, testCreds, suiteRegion, "s3", suiteTime, time.Minute); err == nil {
		t.Error("expected error for missing host")
	}
}

func TestPresignExpiryRange(t *testing.T) {
	r := httptest.NewRequest("GET", "http://h/b/k", nil)
	for _, d := range []time.Duration{0, 500 * time.Millisecond, 8 * 24 * time.Hour} {
		if _, err := Presign(r, testCreds, suiteRegion, "s3", suiteTime, d); err == nil {
			t.Errorf("expiry %s should be rejected", d)
		}
	}
	if _, err := Presign(r, testCreds, suiteRegion, "s3", suiteTime, 7*24*time.Hour); err != nil {
		t.Errorf("7 days should be accepted: %v", err)
	}
}

func TestPresignVerifyErrors(t *testing.T) {
	setQuery := func(r *http.Request, name, value string) {
		pairs := parseRawQuery(r.URL.RawQuery)
		out := make([]queryPair, 0, len(pairs)+1)
		for _, p := range pairs {
			if p.key != name {
				out = append(out, p)
			}
		}
		if value != "" {
			out = append(out, queryPair{name, value})
		}
		r.URL.RawQuery = encodeQuery(out)
	}
	cases := []struct {
		name   string
		now    time.Time
		mutate func(*http.Request)
		verify func(*Verifier)
		want   error
	}{
		{name: "expired", now: suiteTime.Add(time.Hour + time.Second), want: ErrExpired},
		{name: "not yet valid", now: suiteTime.Add(-time.Hour), want: ErrSkew},
		{name: "bad algorithm", mutate: func(r *http.Request) { setQuery(r, queryAlgorithm, "AWS4-ECDSA-P256-SHA256") }, want: ErrUnsupported},
		{name: "missing credential", mutate: func(r *http.Request) { setQuery(r, queryCredential, "") }, want: ErrMalformed},
		{name: "bad date", mutate: func(r *http.Request) { setQuery(r, queryDate, "20150830") }, want: ErrMalformed},
		{name: "missing expires", mutate: func(r *http.Request) { setQuery(r, queryExpires, "") }, want: ErrMalformed},
		{name: "expires zero", mutate: func(r *http.Request) { setQuery(r, queryExpires, "0") }, want: ErrMalformed},
		{name: "expires too long", mutate: func(r *http.Request) { setQuery(r, queryExpires, "604801") }, want: ErrMalformed},
		{name: "expires huge", mutate: func(r *http.Request) { setQuery(r, queryExpires, "99999999999999999999") }, want: ErrMalformed},
		{name: "date mismatch", mutate: func(r *http.Request) { setQuery(r, queryDate, "20150831T123600Z") }, now: suiteTime.Add(24 * time.Hour), want: ErrMalformed},
		{name: "region not allowed", verify: func(v *Verifier) { v.Regions = []string{"eu-west-1"} }, want: ErrMalformed},
		{name: "bad content sha256 header", mutate: func(r *http.Request) { r.Header.Set(headerContentSHA256, "zzz") }, want: ErrMalformed},
		{name: "tampered signature", mutate: func(r *http.Request) {
			setQuery(r, querySignature, strings.Repeat("0", 64))
		}, want: ErrSignatureMismatch},
		{name: "tampered path", mutate: func(r *http.Request) { r.URL.Path = "/b/other" }, want: ErrSignatureMismatch},
		{name: "tampered query", mutate: func(r *http.Request) { setQuery(r, "extra", "1") }, want: ErrSignatureMismatch},
		{name: "unknown key", mutate: func(r *http.Request) {
			setQuery(r, queryCredential, "NOPE/20150830/us-east-1/s3/aws4_request")
		}, want: ErrUnknownKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := presignedRequest(t, "http://localhost:9000/b/k", time.Hour)
			if c.mutate != nil {
				c.mutate(r)
			}
			now := c.now
			if now.IsZero() {
				now = suiteTime
			}
			v := suiteVerifier(now)
			if c.verify != nil {
				c.verify(v)
			}
			_, err := v.Verify(context.Background(), r)
			wantErr(t, err, c.want)
		})
	}
}

func TestPresignedQueryContentSHA256Override(t *testing.T) {
	// A client may pin the payload hash via X-Amz-Content-Sha256 in the query.
	r := httptest.NewRequest("PUT", "http://h/b/k?X-Amz-Content-Sha256="+emptySHA256, nil)
	u, err := Presign(r, testCreds, suiteRegion, "s3", suiteTime, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Presign itself signs UNSIGNED-PAYLOAD, so a verifier honouring the query
	// override must reject this URL; recompute with the override to confirm
	// the override path is used.
	in := httptest.NewRequest("PUT", u.String(), nil)
	_, err = suiteVerifier(suiteTime).Verify(context.Background(), in)
	wantErr(t, err, ErrSignatureMismatch)

	pairs := parseRawQuery(in.URL.RawQuery)
	var kept []queryPair
	for _, p := range pairs {
		if p.key != querySignature {
			kept = append(kept, p)
		}
	}
	in.URL.RawQuery = encodeQuery(kept)
	creq, err := canonicalRequest(in, []string{"host"}, emptySHA256, querySignature)
	if err != nil {
		t.Fatal(err)
	}
	scope := scopeOf(suiteTime, suiteRegion, "s3")
	sig := signHex(signingKey(suiteSecret, suiteTime, suiteRegion, "s3"), stringToSign(suiteTime, scope, creq))
	in.URL.RawQuery = encodeQuery(append(kept, queryPair{querySignature, sig}))
	mustVerify(t, suiteVerifier(suiteTime), in)
}

func TestSignedHeadersWithSemicolonInRawQuery(t *testing.T) {
	// Some clients send the ';' separator unencoded; url.ParseQuery would
	// drop the pair, our parser must not.
	r := httptest.NewRequest("GET", "http://h/b/k", nil)
	r.Header.Set("X-Amz-Meta-A", "1")
	u, err := Presign(r, testCreds, suiteRegion, "s3", suiteTime, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(u.RawQuery, "X-Amz-SignedHeaders=host", "X-Amz-SignedHeaders=host;x-amz-meta-a", 1)
	in := httptest.NewRequest("GET", "http://h/b/k?"+raw, nil)
	in.Header.Set("X-Amz-Meta-A", "1")
	_, err = suiteVerifier(suiteTime).Verify(context.Background(), in)
	// Signature was computed over host only, so it must mismatch rather than
	// fail to parse.
	wantErr(t, err, ErrSignatureMismatch)
	if v, _ := queryLookup(parseRawQuery(raw), querySignedHeaders); v != "host;x-amz-meta-a" {
		t.Errorf("parsed signed headers = %q", v)
	}
}
