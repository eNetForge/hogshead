package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

func TestTokenRoundTrip(t *testing.T) {
	m, err := New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	tok, exp := m.Issue("u-1")
	if !exp.Equal(now.Add(DefaultTTL)) {
		t.Fatalf("expiry %v", exp)
	}
	if id, err := m.Parse(tok); err != nil || id != "u-1" {
		t.Fatalf("parse: %q %v", id, err)
	}
	// Tampered payload or signature, other secret, expired.
	if _, err := m.Parse(tok[:len(tok)-2] + "zz"); !errors.Is(err, ErrInvalid) {
		t.Fatal("tampered signature accepted")
	}
	if _, err := m.Parse("x" + tok); !errors.Is(err, ErrInvalid) {
		t.Fatal("tampered payload accepted")
	}
	other, _ := New([]byte("fedcba9876543210fedcba9876543210"))
	other.Now = m.Now
	if _, err := other.Parse(tok); !errors.Is(err, ErrInvalid) {
		t.Fatal("token from another secret accepted")
	}
	m.Now = func() time.Time { return now.Add(DefaultTTL + time.Second) }
	if _, err := m.Parse(tok); !errors.Is(err, ErrInvalid) {
		t.Fatal("expired token accepted")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short secret accepted")
	}
}

func TestFromRequestChecks(t *testing.T) {
	m, _ := New([]byte("0123456789abcdef0123456789abcdef"))
	tok, _ := m.Issue("u-1")
	req := func(method string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest(method, "http://s3.test/b/k", nil)
		r.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	if id, err := m.FromRequest(httptest.NewRequest("GET", "http://s3.test/", nil)); id != "" || err != nil {
		t.Fatalf("no cookie: %q %v", id, err)
	}
	if id, err := m.FromRequest(req("GET", nil)); id != "u-1" || err != nil {
		t.Fatalf("plain GET: %q %v", id, err)
	}
	if _, err := m.FromRequest(req("PUT", nil)); !errors.Is(err, ErrHeaderRequired) {
		t.Fatalf("PUT without header: %v", err)
	}
	if id, err := m.FromRequest(req("PUT", map[string]string{Header: "1"})); id != "u-1" || err != nil {
		t.Fatalf("PUT with header: %q %v", id, err)
	}
	if _, err := m.FromRequest(req("PUT", map[string]string{Header: "1", "Origin": "http://evil.test"})); !errors.Is(err, ErrCrossSite) {
		t.Fatalf("foreign origin: %v", err)
	}
	if id, err := m.FromRequest(req("PUT", map[string]string{Header: "1", "Origin": "http://s3.test"})); id != "u-1" || err != nil {
		t.Fatalf("own origin: %q %v", id, err)
	}
	if _, err := m.FromRequest(req("GET", map[string]string{"Sec-Fetch-Site": "cross-site"})); !errors.Is(err, ErrCrossSite) {
		t.Fatalf("cross-site fetch metadata: %v", err)
	}
	if id, err := m.FromRequest(req("GET", map[string]string{"Sec-Fetch-Site": "same-origin"})); id != "u-1" || err != nil {
		t.Fatalf("same-origin fetch metadata: %q %v", id, err)
	}
	// Authenticator resolves and rejects disabled users.
	a := &Authenticator{Manager: m, Lookup: func(_ context.Context, id string) (*meta.User, error) {
		if id == "u-1" {
			return &meta.User{ID: id, Name: "alice"}, nil
		}
		return nil, meta.ErrNoSuchUser
	}}
	if u, err := a.UserFromRequest(context.Background(), req("GET", nil)); err != nil || u == nil || u.Name != "alice" {
		t.Fatalf("authenticator: %+v %v", u, err)
	}
	gone, _ := m.Issue("u-2")
	r := httptest.NewRequest("GET", "http://s3.test/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: gone})
	if _, err := a.UserFromRequest(context.Background(), r); !errors.Is(err, ErrInvalid) {
		t.Fatalf("deleted user: %v", err)
	}
	if u, err := a.UserFromRequest(context.Background(), httptest.NewRequest("GET", "http://s3.test/", nil)); u != nil || err != nil {
		t.Fatal("no cookie must be anonymous")
	}
}

func TestCookiesAndLimiter(t *testing.T) {
	rec := httptest.NewRecorder()
	SetCookie(rec, "tok", time.Now().Add(time.Hour), true)
	c := rec.Result().Cookies()[0]
	if c.Name != CookieName || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie attrs: %+v", c)
	}
	rec = httptest.NewRecorder()
	ClearCookie(rec, false)
	if c := rec.Result().Cookies()[0]; c.MaxAge != -1 || c.Value != "" {
		t.Fatalf("clear: %+v", c)
	}
	l := NewLimiter(3, time.Minute)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if l.Blocked("1.2.3.4", now) {
			t.Fatal("blocked early")
		}
		l.Fail("1.2.3.4", now)
	}
	if !l.Blocked("1.2.3.4", now) || l.Blocked("5.6.7.8", now) {
		t.Fatal("limiter")
	}
	if l.Blocked("1.2.3.4", now.Add(2*time.Minute)) {
		t.Fatal("window did not expire")
	}
	l.Fail("1.2.3.4", now)
	l.Reset("1.2.3.4")
	if l.Blocked("1.2.3.4", now) {
		t.Fatal("reset")
	}
}

func TestRotateInvalidatesIssuedTokens(t *testing.T) {
	m, err := New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	old, _ := m.Issue("u-1")
	if err := m.Rotate([]byte("short")); err == nil {
		t.Fatal("short secret accepted")
	}
	if _, err := m.Parse(old); err != nil {
		t.Fatalf("a refused rotation must keep the old secret: %v", err)
	}
	if err := m.Rotate([]byte("fedcba9876543210fedcba9876543210")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Parse(old); !errors.Is(err, ErrInvalid) {
		t.Fatal("token issued before the rotation still verifies")
	}
	fresh, _ := m.Issue("u-1")
	if id, err := m.Parse(fresh); err != nil || id != "u-1" {
		t.Fatalf("token issued after the rotation: %q %v", id, err)
	}
}
