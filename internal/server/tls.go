package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync/atomic"

	"github.com/enetforge/hogshead/internal/acme"
)

// TLSProvider returns a function yielding the current TLS config for the S3
// listeners, or nil when TLS is off. The returned config reloads certificates
// on change through the live config pointer.
func TLSProvider(ctx context.Context, core *Core) (func() *tls.Config, error) {
	cfg := core.Cfg()
	switch cfg.TLS.Mode {
	case "off":
		return func() *tls.Config { return nil }, nil
	case "file":
		var cert atomic.Pointer[tls.Certificate]
		load := func() error {
			c, err := tls.LoadX509KeyPair(core.Cfg().TLS.CertFile, core.Cfg().TLS.KeyFile)
			if err != nil {
				return fmt.Errorf("load tls cert: %w", err)
			}
			cert.Store(&c)
			return nil
		}
		if err := load(); err != nil {
			return nil, err
		}
		core.OnReload(func() {
			if err := load(); err != nil {
				core.Log.Error("reload tls cert failed", "err", err)
			} else {
				core.Log.Info("tls certificate reloaded")
			}
		})
		tc := &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return cert.Load(), nil
			},
		}
		return func() *tls.Config { return tc }, nil
	case "acme":
		return acmeProvider(ctx, core)
	}
	return nil, fmt.Errorf("unknown tls mode %q", cfg.TLS.Mode)
}

func acmeProvider(ctx context.Context, core *Core) (func() *tls.Config, error) {
	cfg := core.Cfg()
	storage := acme.NewStorage(core.DB, core.ProposeRaw, nil)
	p, err := acme.New(ctx, acme.Options{
		Email: cfg.TLS.ACMEEmail, CA: cfg.TLS.ACMECA, Domains: cfg.ACMEDomains(),
		DNS: cfg.TLS.DNS.Provider, DNSSettings: cfg.TLS.DNS.Settings, Storage: storage, Logger: core.Log,
	})
	if err != nil {
		return nil, fmt.Errorf("acme: %w", err)
	}
	core.ACME = p
	core.OnReload(func() {
		if err := p.Update(ctx, core.Cfg().ACMEDomains()); err != nil {
			core.Log.Error("acme domain update failed", "err", err)
		}
	})
	tc := p.TLSConfig()
	return func() *tls.Config { return tc }, nil
}
