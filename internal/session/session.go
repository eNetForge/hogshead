// Package session issues and verifies the HMAC-signed cookie that logs a
// browser into the web UI with a username and password instead of an access
// key. Requests carrying the cookie are treated as the user, subject to
// same-origin checks that stop other sites from riding on it.
package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

const (
	// CookieName carries the session token.
	CookieName = "hogshead_session"
	// Header must accompany every state-changing request authenticated by
	// the cookie. Cross-site forms and simple requests cannot set it.
	Header = "X-Hogshead-Session"
	// SecretSetting holds the hex-encoded signing secret in server settings.
	SecretSetting = "ui.session_secret" //nolint:gosec // a setting name, not a credential
	// DefaultTTL is how long a login lasts.
	DefaultTTL = 7 * 24 * time.Hour
)

var (
	// ErrInvalid is a tampered, malformed or expired token.
	ErrInvalid = errors.New("invalid or expired session")
	// ErrCrossSite is a cookie-authenticated request that did not originate
	// from this server's own pages.
	ErrCrossSite = errors.New("cross-site request refused")
	// ErrHeaderRequired is a state-changing cookie request without Header.
	ErrHeaderRequired = errors.New("session requests must carry the " + Header + " header")
)

// Manager signs and verifies tokens.
type Manager struct {
	secret []byte
	Now    func() time.Time
	TTL    time.Duration
}

// New builds a manager from a signing secret (at least 32 bytes).
func New(secret []byte) (*Manager, error) {
	if len(secret) < 32 {
		return nil, errors.New("session secret must be at least 32 bytes")
	}
	return &Manager{secret: append([]byte(nil), secret...), Now: time.Now, TTL: DefaultTTL}, nil
}

// NewSecret returns a random hex secret suitable for SecretSetting.
func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Issue creates a token for a user.
func (m *Manager) Issue(userID string) (string, time.Time) {
	exp := m.Now().Add(m.TTL)
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	payload := userID + "|" + strconv.FormatInt(exp.Unix(), 10) + "|" + hex.EncodeToString(nonce)
	return b64(payload) + "." + b64(string(m.sign(payload))), exp
}

// Parse verifies a token and returns its user id.
func (m *Manager) Parse(token string) (string, error) {
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", ErrInvalid
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return "", ErrInvalid
	}
	if subtle.ConstantTimeCompare(mac, m.sign(string(payload))) != 1 {
		return "", ErrInvalid
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 3 || parts[0] == "" {
		return "", ErrInvalid
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !m.Now().Before(time.Unix(exp, 0)) {
		return "", ErrInvalid
	}
	return parts[0], nil
}

func (m *Manager) sign(payload string) []byte {
	h := hmac.New(sha256.New, m.secret)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// SetCookie writes the session cookie. secure should be true behind TLS.
func SetCookie(w http.ResponseWriter, token string, expires time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the TLS mode: a Secure cookie is never sent over plain HTTP
		Name: CookieName, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie expires the session cookie.
func ClearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see SetCookie
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	})
}

// FromRequest returns the user id of a valid session cookie, or "" when the
// request carries none. A cookie that fails verification or a request that
// fails the same-origin checks returns an error.
func (m *Manager) FromRequest(r *http.Request) (string, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return "", nil
	}
	if err := sameOrigin(r); err != nil {
		return "", err
	}
	return m.Parse(c.Value)
}

// sameOrigin rejects cookie-authenticated requests that another site could
// have triggered: cross-site fetch metadata, a foreign Origin, or a
// state-changing method without the custom header.
func sameOrigin(r *http.Request) error {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return ErrCrossSite
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return ErrCrossSite
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if r.Header.Get(Header) == "" {
		return ErrHeaderRequired
	}
	return nil
}

// UserLookup resolves a user id.
type UserLookup func(ctx context.Context, id string) (*meta.User, error)

// Authenticator turns a cookie into a user for the S3 and admin handlers.
type Authenticator struct {
	Manager *Manager
	Lookup  UserLookup
}

// UserFromRequest returns (nil, nil) without a cookie, the user for a valid
// one, and an error for a bad cookie, a disabled or deleted user, or a
// request that fails the same-origin checks.
func (a *Authenticator) UserFromRequest(ctx context.Context, r *http.Request) (*meta.User, error) {
	id, err := a.Manager.FromRequest(r)
	if err != nil || id == "" {
		return nil, err
	}
	u, err := a.Lookup(ctx, id)
	if err != nil {
		return nil, ErrInvalid
	}
	if u.Disabled {
		return nil, fmt.Errorf("%w: user disabled", ErrInvalid)
	}
	return u, nil
}

// Limiter throttles password guesses per client address.
type Limiter struct {
	mu     sync.Mutex
	fails  map[string][]time.Time
	Burst  int
	Window time.Duration
}

// NewLimiter allows burst failures per window per address.
func NewLimiter(burst int, window time.Duration) *Limiter {
	return &Limiter{fails: map[string][]time.Time{}, Burst: burst, Window: window}
}

// Blocked reports whether addr has exhausted its attempts.
func (l *Limiter) Blocked(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(addr, now)) >= l.Burst
}

// Fail records a failed attempt.
func (l *Limiter) Fail(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[addr] = append(l.prune(addr, now), now)
}

// Reset forgets an address after a successful login.
func (l *Limiter) Reset(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, addr)
}

func (l *Limiter) prune(addr string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.fails[addr] {
		if now.Sub(t) < l.Window {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, addr)
	} else {
		l.fails[addr] = keep
	}
	return keep
}
