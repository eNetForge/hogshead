package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const s3ExampleAuth = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
	"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date," +
	"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"

func s3ExampleRequest(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", "/test.txt", nil)
	r.Host = "examplebucket.s3.amazonaws.com"
	r.Header.Set("Range", "bytes=0-9")
	r.Header.Set(headerContentSHA256, emptySHA256)
	r.Header.Set(headerAmzDate, "20130524T000000Z")
	r.Header.Set(headerAuthorization, s3ExampleAuth)
	return r
}

func TestVerifyS3DocExample(t *testing.T) {
	v := suiteVerifier(s3ExampleTime.Add(5 * time.Minute))
	id := mustVerify(t, v, s3ExampleRequest(t))
	if id.AccessKeyID != s3ExampleAccessKey || id.Region != "us-east-1" || id.Service != "s3" {
		t.Errorf("identity = %+v", id)
	}
	if id.Payload != PayloadSigned || id.ContentSHA256 != emptySHA256 {
		t.Errorf("payload = %v %q", id.Payload, id.ContentSHA256)
	}
	if len(id.SignedHeaders) != 4 || id.SignedHeaders[1] != "range" {
		t.Errorf("signed headers = %v", id.SignedHeaders)
	}
	if len(id.SigningKey) != 32 || id.SeedSignature != "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41" {
		t.Errorf("key/seed = %x %s", id.SigningKey, id.SeedSignature)
	}
	if id.Scope != "20130524/us-east-1/s3/aws4_request" {
		t.Errorf("scope = %s", id.Scope)
	}
}

func TestVerifySpacingAndCaseTolerance(t *testing.T) {
	v := suiteVerifier(s3ExampleTime)
	r := s3ExampleRequest(t)
	r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256   Credential = AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request ,  SignedHeaders=host;range;x-amz-content-sha256;x-amz-date ,Signature=F0E8BDB87C964420E857BD35B5D6ED310BD44F0170ABA48DD91039C6036BDB41, Extra=ignored")
	mustVerify(t, v, r)
}

func TestVerifyDateHeaderFallback(t *testing.T) {
	v := suiteVerifier(suiteTime)
	r := suiteRequest(t, "GET", "/", nil)
	r.Header.Del(headerAmzDate)
	r.Header.Set("Date", suiteTime.Format(http.TimeFormat))
	if err := Sign(r, Credentials{suiteAccessKey, suiteSecret}, suiteRegion, suiteService, suiteTime, ""); err != nil {
		t.Fatal(err)
	}
	// Sign always adds X-Amz-Date; drop it from the request but keep it out
	// of SignedHeaders by signing a request that never had it.
	r2 := suiteRequest(t, "GET", "/", nil)
	r2.Header.Del(headerAmzDate)
	r2.Header.Set("Date", suiteTime.Format(http.TimeFormat))
	r2.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=date;host, Signature="+
		signFor(t, r2, []string{"date", "host"}, suiteTime))
	id := mustVerify(t, v, r2)
	if !id.Date.Equal(suiteTime) {
		t.Errorf("date = %v", id.Date)
	}
}

// signFor computes the expected signature for r with the suite credentials.
func signFor(t *testing.T, r *http.Request, signed []string, now time.Time) string {
	t.Helper()
	hash := r.Header.Get(headerContentSHA256)
	if hash == "" {
		hash = unsignedPayload
	}
	creq, err := canonicalRequest(r, signed, hash, "")
	if err != nil {
		t.Fatal(err)
	}
	scope := scopeOf(now, suiteRegion, suiteService)
	return signHex(signingKey(suiteSecret, now, suiteRegion, suiteService), stringToSign(now, scope, creq))
}

func TestVerifyErrors(t *testing.T) {
	base := func(t *testing.T) *http.Request { return s3ExampleRequest(t) }
	cases := []struct {
		name   string
		mutate func(*testing.T, *http.Request)
		now    time.Time
		verify func(*Verifier)
		want   error
	}{
		{name: "missing auth", mutate: func(_ *testing.T, r *http.Request) { r.Header.Del(headerAuthorization) }, want: ErrMissingAuth},
		{name: "sigv2 header", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS AKIAIOSFODNN7EXAMPLE:frJIUN8DYpKDtOLCwo//yllqDzg=")
		}, want: ErrUnsupported},
		{name: "bearer", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set(headerAuthorization, "Bearer abc") }, want: ErrUnsupported},
		{name: "sigv2 query", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Del(headerAuthorization)
			r.URL.RawQuery = "AWSAccessKeyId=AKIAIOSFODNN7EXAMPLE&Signature=abc&Expires=1"
		}, want: ErrUnsupported},
		{name: "signature without algorithm", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Del(headerAuthorization)
			r.URL.RawQuery = "X-Amz-Signature=abc"
		}, want: ErrMalformed},
		{name: "component without equals", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential, Signature=abc")
		}, want: ErrMalformed},
		{name: "missing credential", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 SignedHeaders=host, Signature=abc")
		}, want: ErrMalformed},
		{name: "credential wrong parts", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential=AK/20130524/us-east-1/s3, SignedHeaders=host, Signature="+emptySHA256)
		}, want: ErrMalformed},
		{name: "credential empty region", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential=AK/20130524//s3/aws4_request, SignedHeaders=host, Signature="+emptySHA256)
		}, want: ErrMalformed},
		{name: "credential bad date", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential=AK/2013052/us-east-1/s3/aws4_request, SignedHeaders=host, Signature="+emptySHA256)
		}, want: ErrMalformed},
		{name: "empty signed headers", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential=AK/20130524/us-east-1/s3/aws4_request, SignedHeaders=, Signature="+emptySHA256)
		}, want: ErrMalformed},
		{name: "empty signed header entry", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential=AK/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;;range, Signature="+emptySHA256)
		}, want: ErrMalformed},
		{name: "non hex signature", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential=AK/20130524/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=xyz")
		}, want: ErrMalformed},
		{name: "missing date", mutate: func(_ *testing.T, r *http.Request) { r.Header.Del(headerAmzDate) }, want: ErrMalformed},
		{name: "bad amz date", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set(headerAmzDate, "2013-05-24") }, want: ErrMalformed},
		{name: "bad date header", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Del(headerAmzDate)
			r.Header.Set("Date", "yesterday")
		}, want: ErrMalformed},
		{name: "credential date mismatch", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set(headerAmzDate, "20130525T000000Z") }, now: s3ExampleTime.Add(24 * time.Hour), want: ErrMalformed},
		{name: "region not allowed", verify: func(v *Verifier) { v.Regions = []string{"eu-central-1"} }, want: ErrMalformed},
		{name: "bad content sha256", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set(headerContentSHA256, "nope") }, want: ErrMalformed},
		{name: "ecdsa streaming unsupported", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerContentSHA256, "STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD")
		}, want: ErrUnsupported},
		{name: "skew past", now: s3ExampleTime.Add(16 * time.Minute), want: ErrSkew},
		{name: "skew future", now: s3ExampleTime.Add(-16 * time.Minute), want: ErrSkew},
		{name: "custom skew", now: s3ExampleTime.Add(2 * time.Minute), verify: func(v *Verifier) { v.Skew = time.Minute }, want: ErrSkew},
		{name: "unknown key", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, "AWS4-HMAC-SHA256 Credential=NOPE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature="+emptySHA256)
		}, want: ErrUnknownKey},
		{name: "empty secret is unknown", verify: func(v *Verifier) {
			v.Lookup = func(context.Context, string) (string, error) { return "", nil }
		}, want: ErrUnknownKey},
		{name: "tampered signature", mutate: func(_ *testing.T, r *http.Request) {
			r.Header.Set(headerAuthorization, s3ExampleAuth[:len(s3ExampleAuth)-1]+"0")
		}, want: ErrSignatureMismatch},
		{name: "tampered header", mutate: func(_ *testing.T, r *http.Request) { r.Header.Set("Range", "bytes=0-10") }, want: ErrSignatureMismatch},
		{name: "tampered path", mutate: func(_ *testing.T, r *http.Request) { r.URL.Path = "/other.txt" }, want: ErrSignatureMismatch},
		{name: "tampered host", mutate: func(_ *testing.T, r *http.Request) { r.Host = "other.example" }, want: ErrSignatureMismatch},
		{name: "missing signed header", mutate: func(_ *testing.T, r *http.Request) { r.Header.Del("Range") }, want: ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := c.now
			if now.IsZero() {
				now = s3ExampleTime
			}
			v := suiteVerifier(now)
			if c.verify != nil {
				c.verify(v)
			}
			r := base(t)
			if c.mutate != nil {
				c.mutate(t, r)
			}
			_, err := v.Verify(context.Background(), r)
			wantErr(t, err, c.want)
		})
	}
}

func TestVerifyLookupFailure(t *testing.T) {
	boom := errors.New("database down")
	v := &Verifier{
		Lookup: func(context.Context, string) (string, error) { return "", boom },
		Now:    func() time.Time { return s3ExampleTime },
	}
	_, err := v.Verify(context.Background(), s3ExampleRequest(t))
	wantErr(t, err, boom)
	if errors.Is(err, ErrUnknownKey) {
		t.Error("infrastructure error must not be reported as unknown key")
	}
}

func TestVerifyNilLookup(t *testing.T) {
	v := &Verifier{Now: func() time.Time { return s3ExampleTime }}
	if _, err := v.Verify(context.Background(), s3ExampleRequest(t)); err == nil {
		t.Fatal("expected error for nil Lookup")
	}
}

func TestVerifyNilRequest(t *testing.T) {
	v := suiteVerifier(s3ExampleTime)
	_, err := v.Verify(context.Background(), nil)
	wantErr(t, err, ErrMalformed)
}

func TestVerifyDefaultNow(t *testing.T) {
	v := &Verifier{Lookup: testKeys(map[string]string{suiteAccessKey: suiteSecret})}
	r := httptest.NewRequest("GET", "http://localhost:9000/bucket/key", nil)
	if err := Sign(r, Credentials{suiteAccessKey, suiteSecret}, suiteRegion, "s3", time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	mustVerify(t, v, r)
}

func TestVerifyMissingContentSHA256IsUnsigned(t *testing.T) {
	v := suiteVerifier(suiteTime)
	r := suiteRequest(t, "PUT", "/bucket/key", nil)
	if err := Sign(r, Credentials{suiteAccessKey, suiteSecret}, suiteRegion, suiteService, suiteTime, ""); err != nil {
		t.Fatal(err)
	}
	id := mustVerify(t, v, r)
	if id.Payload != PayloadUnsigned || id.ContentSHA256 != "" {
		t.Errorf("payload = %v %q", id.Payload, id.ContentSHA256)
	}
}
