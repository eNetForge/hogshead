package acme

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/v3/acme"
)

// brokenCA is an ACME directory endpoint that always fails, so certmagic
// never gets past the first request and nothing leaves the host.
func brokenCA(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestProvider(t *testing.T, o Options) (*Provider, context.CancelFunc) {
	t.Helper()
	s, _, _ := newStorage(t)
	if o.Storage == nil {
		o.Storage = s
	}
	if o.Logger == nil {
		o.Logger = discardLogger()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p, err := New(ctx, o)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); p.Close() })
	return p, cancel
}

func TestNewValidation(t *testing.T) {
	s, _, _ := newStorage(t)
	ctx := context.Background()
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"no storage", Options{Domains: []string{"a.example"}}, "storage is required"},
		{"no domains", Options{Storage: s}, "at least one domain"},
		{"empty domain", Options{Storage: s, Domains: []string{"a.example", " "}}, "empty domain"},
		{"invalid domain", Options{Storage: s, Domains: []string{"a.example/x"}}, "invalid domain"},
		{"bad CA", Options{Storage: s, Domains: []string{"a.example"}, CA: "not a url"}, "invalid CA"},
		{"ftp CA", Options{Storage: s, Domains: []string{"a.example"}, CA: "ftp://ca/dir"}, "invalid CA"},
		{"unknown dns", Options{Storage: s, Domains: []string{"a.example"}, DNS: "namecheap"}, "unknown dns provider"},
		{"dns missing setting", Options{Storage: s, Domains: []string{"a.example"}, DNS: "cloudflare"}, `requires setting "api_token"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Logger = discardLogger()
			p, err := New(ctx, tc.opts)
			if err == nil {
				p.Close()
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
	//nolint:staticcheck // deliberately passing a nil context
	if _, err := New(nil, Options{Storage: s, Domains: []string{"a.example"}}); err == nil {
		t.Fatal("nil ctx: want error")
	}
}

func TestNewWithFailingCAStillConstructs(t *testing.T) {
	ca, hits := brokenCA(t)
	p, _ := newTestProvider(t, Options{
		Email:   "ops@example.com",
		CA:      ca.URL,
		Domains: []string{"S3.Example.com", "s3.example.com", "*.s3.example.com"},
	})

	tc := p.TLSConfig()
	if tc == nil || tc.GetCertificate == nil {
		t.Fatal("TLSConfig must be non-nil with GetCertificate set")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x", tc.MinVersion)
	}
	if len(tc.NextProtos) < 3 || tc.NextProtos[0] != "h2" || tc.NextProtos[1] != "http/1.1" || tc.NextProtos[2] != "acme-tls/1" {
		t.Fatalf("NextProtos = %v", tc.NextProtos)
	}
	if got := p.Domains(); !reflect.DeepEqual(got, []string{"*.s3.example.com", "s3.example.com"}) {
		t.Fatalf("Domains() = %v", got)
	}

	// The background goroutines reach the (local) CA and fail; they must
	// not take the constructor down with them.
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("ManageAsync never contacted the CA")
	}
}

func TestNewWithDNSProvider(t *testing.T) {
	ca, _ := brokenCA(t)
	p, _ := newTestProvider(t, Options{
		CA:          ca.URL,
		Domains:     []string{"*.s3.example.com"},
		DNS:         "cloudflare",
		DNSSettings: map[string]string{"api_token": "test-token"},
	})
	if p.issuer.DNS01Solver == nil {
		t.Fatal("DNS01Solver not configured")
	}
}

func TestDefaultCAIsLetsEncrypt(t *testing.T) {
	got, err := resolveCA("")
	if err != nil || !strings.Contains(got, "letsencrypt.org") {
		t.Fatalf("resolveCA(\"\") = %q, %v", got, err)
	}
}

func TestHTTPChallengeHandler(t *testing.T) {
	ca, _ := brokenCA(t)
	s, _, _ := newStorage(t)
	p, _ := newTestProvider(t, Options{CA: ca.URL, Domains: []string{"s3.example.com"}, Storage: s})

	var nextHit atomic.Int32
	h := p.HTTPChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextHit.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))

	// Ordinary S3 traffic is passed through untouched.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://s3.example.com/bucket/key", nil))
	if rec.Code != http.StatusTeapot || nextHit.Load() != 1 {
		t.Fatalf("ordinary request: code %d hits %d", rec.Code, nextHit.Load())
	}

	// A challenge nobody in the cluster initiated is not ours to answer.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://s3.example.com/.well-known/acme-challenge/unknown", nil))
	if nextHit.Load() != 2 {
		t.Fatalf("unknown challenge should fall through to next")
	}

	// Another node initiated a challenge and recorded it in the shared
	// Storage; this node must answer it (distributed HTTP-01 solving).
	chal := acme.Challenge{
		Type:             acme.ChallengeTypeHTTP01,
		Token:            "tok123",
		KeyAuthorization: "tok123.thumbprint",
		Identifier:       acme.Identifier{Type: "dns", Value: "s3.example.com"},
	}
	body, err := json.Marshal(chal)
	if err != nil {
		t.Fatal(err)
	}
	key := path.Join("acme", certmagic.StorageKeys.Safe(p.issuer.IssuerKey()), "challenge_tokens", certmagic.StorageKeys.Safe("s3.example.com")+".json")
	if err := s.Store(context.Background(), key, body); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://s3.example.com"+chal.HTTP01ResourcePath(), nil))
	if rec.Code != http.StatusOK || rec.Body.String() != chal.KeyAuthorization || nextHit.Load() != 2 {
		t.Fatalf("distributed challenge: code %d body %q hits %d", rec.Code, rec.Body.String(), nextHit.Load())
	}
}

func TestUpdateDomains(t *testing.T) {
	ca, _ := brokenCA(t)
	p, _ := newTestProvider(t, Options{CA: ca.URL, Domains: []string{"a.example.com"}})
	ctx := context.Background()

	if err := p.Update(ctx, []string{"a.example.com", "B.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := p.Domains(); !reflect.DeepEqual(got, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("after add: %v", got)
	}
	if err := p.Update(ctx, []string{"b.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := p.Domains(); !reflect.DeepEqual(got, []string{"b.example.com"}) {
		t.Fatalf("after remove: %v", got)
	}
	// No-op update.
	if err := p.Update(ctx, []string{"b.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Update(ctx, nil); err == nil {
		t.Fatal("empty update should fail")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.Update(cancelled, []string{"c.example.com"}); err == nil {
		t.Fatal("cancelled ctx should fail")
	}
}
