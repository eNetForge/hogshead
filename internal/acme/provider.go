package acme

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/caddyserver/certmagic"
)

// Options configures a Provider.
type Options struct {
	Email       string            // ACME account contact; optional
	CA          string            // directory URL; "" = certmagic.LetsEncryptProductionCA
	Domains     []string          // names to obtain certificates for; at least one
	DNS         string            // libdns provider name (see DNSProviders); "" disables DNS-01
	DNSSettings map[string]string // provider credentials, see dns.go
	Storage     certmagic.Storage // shared certificate store; required
	Logger      *slog.Logger      // nil = slog.Default()
}

// Provider wires certmagic for the configured domains and hands out a
// *tls.Config whose GetCertificate serves the managed certificates.
// Certificates are obtained via HTTP-01 (and TLS-ALPN-01 when the TLS
// listener is on 443) and, when a DNS provider is configured, DNS-01, which
// is required for wildcard names.
type Provider struct {
	ctx    context.Context // lifetime of background management
	cfg    *certmagic.Config
	cache  *certmagic.Cache
	issuer *certmagic.ACMEIssuer
	log    *slog.Logger

	mu      sync.Mutex
	domains map[string]struct{} // currently managed, normalised
}

// New validates o, builds a certmagic configuration with its own
// certificate cache and starts managing o.Domains asynchronously. ctx bounds
// all background work (obtaining, renewing, retries); cancel it on shutdown.
// Errors are logged through o.Logger; New returns before any certificate is
// ready.
func New(ctx context.Context, o Options) (*Provider, error) {
	if ctx == nil {
		return nil, errors.New("acme: nil context")
	}
	if o.Storage == nil {
		return nil, errors.New("acme: storage is required")
	}
	domains, err := normalizeDomains(o.Domains)
	if err != nil {
		return nil, err
	}
	ca, err := resolveCA(o.CA)
	if err != nil {
		return nil, err
	}
	var dns certmagic.DNSProvider
	if o.DNS != "" {
		if dns, err = newDNSProvider(o.DNS, o.DNSSettings); err != nil {
			return nil, err
		}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	zl := zapLogger(o.Logger)

	p := &Provider{ctx: ctx, log: o.Logger, domains: map[string]struct{}{}}
	p.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return p.cfg, nil },
		Logger:           zl,
	})
	p.cfg = certmagic.New(p.cache, certmagic.Config{Storage: o.Storage, Logger: zl})
	tmpl := certmagic.ACMEIssuer{CA: ca, Email: o.Email, Agreed: true, Logger: zl}
	if dns != nil {
		tmpl.DNS01Solver = &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{DNSProvider: dns, Logger: zl}}
	}
	p.issuer = certmagic.NewACMEIssuer(p.cfg, tmpl)
	p.cfg.Issuers = []certmagic.Issuer{p.issuer}

	if err := p.manage(domains); err != nil {
		p.cache.Stop()
		return nil, err
	}
	return p, nil
}

// TLSConfig returns a server TLS configuration backed by the managed
// certificates. It requires TLS 1.2+ and advertises h2 and HTTP/1.1 ahead
// of the ACME TLS-ALPN protocol.
func (p *Provider) TLSConfig() *tls.Config {
	tc := p.cfg.TLSConfig()
	tc.MinVersion = tls.VersionTLS12
	tc.NextProtos = append([]string{"h2", "http/1.1"}, tc.NextProtos...)
	return tc
}

// HTTPChallengeHandler wraps next so requests under
// /.well-known/acme-challenge/ are answered for this node or any other node
// sharing the Storage; everything else is passed to next. Serve it on port
// 80 so HTTP-01 succeeds without certmagic opening its own listener.
func (p *Provider) HTTPChallengeHandler(next http.Handler) http.Handler {
	return p.issuer.HTTPChallengeHandler(next)
}

// Domains returns the currently managed names, sorted.
func (p *Provider) Domains() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.domains))
	for d := range p.domains {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Update replaces the managed domain set: names not yet managed are started
// (asynchronously, bound to the ctx given to New); names no longer listed
// stop being renewed. ctx is only used for validation of the call itself.
func (p *Provider) Update(ctx context.Context, domains []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	want, err := normalizeDomains(domains)
	if err != nil {
		return err
	}
	return p.manage(want)
}

// manage reconciles p.domains with want.
func (p *Provider) manage(want []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make(map[string]struct{}, len(want))
	var added []string
	for _, d := range want {
		next[d] = struct{}{}
		if _, ok := p.domains[d]; !ok {
			added = append(added, d)
		}
	}
	var removed []certmagic.SubjectIssuer
	for d := range p.domains {
		if _, ok := next[d]; !ok {
			removed = append(removed, certmagic.SubjectIssuer{Subject: d, IssuerKey: p.issuer.IssuerKey()})
		}
	}
	if len(added) > 0 {
		if err := p.cfg.ManageAsync(p.ctx, added); err != nil {
			return fmt.Errorf("acme: manage %v: %w", added, err)
		}
		p.log.Info("acme: managing certificates", "domains", added)
	}
	if len(removed) > 0 {
		p.cache.RemoveManaged(removed)
		p.log.Info("acme: stopped managing certificates", "count", len(removed))
	}
	p.domains = next
	return nil
}

// Close stops the certificate cache's maintenance goroutines. Background
// issuance stops when the ctx passed to New is cancelled.
func (p *Provider) Close() {
	p.cache.Stop()
}

// normalizeDomains lower-cases, trims and de-duplicates names, rejecting
// empty ones and an empty list.
func normalizeDomains(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("acme: at least one domain is required")
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		n := strings.ToLower(strings.TrimSpace(d))
		if n == "" {
			return nil, errors.New("acme: empty domain name")
		}
		if strings.ContainsAny(n, " /\\@") {
			return nil, fmt.Errorf("acme: invalid domain name %q", d)
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out, nil
}

// resolveCA validates the directory URL, defaulting to Let's Encrypt.
func resolveCA(ca string) (string, error) {
	ca = strings.TrimSpace(ca)
	if ca == "" {
		return certmagic.LetsEncryptProductionCA, nil
	}
	u, err := url.Parse(ca)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("acme: invalid CA directory URL %q", ca)
	}
	return ca, nil
}
