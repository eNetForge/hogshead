package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Vectors from the AWS SigV4 test suite (aws-sig-v4-test-suite).
const (
	suiteAccessKey = "AKIDEXAMPLE"
	suiteSecret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	suiteHost      = "example.amazonaws.com"
	suiteDate      = "20150830T123600Z"
	suiteRegion    = "us-east-1"
	suiteService   = "service"

	// The S3 GetObject example from the AWS documentation.
	s3ExampleAccessKey = "AKIAIOSFODNN7EXAMPLE"
	s3ExampleSecret    = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var (
	suiteTime     = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	s3ExampleTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
)

// testKeys is a SecretLookup over a fixed map.
func testKeys(keys map[string]string) SecretLookup {
	return func(_ context.Context, ak string) (string, error) {
		if s, ok := keys[ak]; ok {
			return s, nil
		}
		return "", ErrUnknownKey
	}
}

func suiteVerifier(now time.Time) *Verifier {
	return &Verifier{
		Lookup: testKeys(map[string]string{
			suiteAccessKey:     suiteSecret,
			s3ExampleAccessKey: s3ExampleSecret,
		}),
		Now: func() time.Time { return now },
	}
}

func suiteRequest(t *testing.T, method, target string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = suiteHost
	r.Header.Set(headerAmzDate, suiteDate)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func mustVerify(t *testing.T, v *Verifier, r *http.Request) *Identity {
	t.Helper()
	id, err := v.Verify(context.Background(), r)
	if err != nil {
		t.Fatalf("Verify: unexpected error: %v", err)
	}
	return id
}

func wantErr(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got error %v, want %v", got, want)
	}
}
