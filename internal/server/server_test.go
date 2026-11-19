package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/meta"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func writeCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cp := filepath.Join(dir, "cert.pem")
	kp := filepath.Join(dir, "key.pem")
	_ = os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	kb, _ := x509.MarshalECPrivateKey(key)
	_ = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cp, kp
}

func TestOpenServeReloadTLS(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default(dir)
	cfg.Listen = []string{freePort(t)}
	cfg.Admin.Socket = "" // path would be too long for a unix socket in temp dirs
	cp, kp := writeCert(t, dir)
	cfg.TLS = config.TLS{Mode: "file", CertFile: cp, KeyFile: kp}
	if err := WriteMasterKey(cfg.MasterKeyPath()); err != nil {
		t.Fatal(err)
	}
	if err := WriteMasterKey(cfg.MasterKeyPath()); err == nil {
		t.Fatal("must not overwrite key")
	}
	cfgPath := filepath.Join(dir, "config.toml")
	b, _ := config.Marshal(cfg)
	_ = os.WriteFile(cfgPath, b, 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := Open(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if !core.Encrypted || !core.FS.Encrypted() {
		t.Fatal("encryption should be on")
	}
	if _, err := core.Propose(ctx, &meta.CreateBucket{Name: "b", Owner: "u", Now: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Propose(ctx, &meta.CreateBucket{Name: "b", Owner: "u", Now: 1}); err == nil {
		t.Fatal("business error must surface")
	}
	tlsConf, err := TLSProvider(ctx, core)
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hi")) })
	ln := NewListeners(core, h, h, tlsConf)
	done := make(chan error, 1)
	go func() { done <- ln.Serve(ctx) }()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec
	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = client.Get("https://" + cfg.Listen[0] + "/")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hi" {
		t.Fatal("tls serve")
	}
	// Reload: change a live setting and a restart-only one.
	cfg2 := cfg
	cfg2.LogLevel = "debug"
	cfg2.Listen = []string{"127.0.0.1:1"}
	b, _ = config.Marshal(cfg2)
	_ = os.WriteFile(cfgPath, b, 0o600)
	hook := 0
	core.OnReload(func() { hook++ })
	if err := core.Reload(cfgPath); err != nil {
		t.Fatal(err)
	}
	if core.Cfg().LogLevel != "debug" || core.Cfg().Listen[0] != cfg.Listen[0] || hook != 1 {
		t.Fatalf("reload: %+v", core.Cfg())
	}
	if p := core.PendingRestart(); len(p) != 1 || p[0] != "listen" {
		t.Fatalf("pending: %v", p)
	}
	// Invalid file: reload fails, config unchanged.
	_ = os.WriteFile(cfgPath, []byte("data_dir = 1\n"), 0o600)
	if err := core.Reload(cfgPath); err == nil {
		t.Fatal("bad reload")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default(dir)
	kp := cfg.MasterKeyPath()
	_ = os.WriteFile(kp, []byte("short"), 0o600)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := Open(context.Background(), cfg, log); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("short key: %v", err)
	}
	_ = os.WriteFile(kp, make([]byte, 32), 0o600)
	_ = os.Chmod(kp, 0o644)
	if _, err := Open(context.Background(), cfg, log); err == nil || !strings.Contains(err.Error(), "readable") {
		t.Fatalf("perms: %v", err)
	}
	cfg.TLS = config.TLS{Mode: "file", CertFile: "/nope", KeyFile: "/nope"}
	_ = os.Remove(kp)
	core, err := Open(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, err := TLSProvider(context.Background(), core); err == nil {
		t.Fatal("missing cert")
	}
	cfg.TLS.Mode = "weird"
	core.Config.Store(&cfg)
	if _, err := TLSProvider(context.Background(), core); err == nil {
		t.Fatal("unknown mode")
	}
	cfg.TLS.Mode = "off"
	core.Config.Store(&cfg)
	tc, _ := TLSProvider(context.Background(), core)
	if tc() != nil {
		t.Fatal("off")
	}
	cfg.Listen = []string{"300.300.300.300:1"}
	core.Config.Store(&cfg)
	if err := NewListeners(core, nil, nil, tc).Serve(context.Background()); err == nil {
		t.Fatal("bad listen must fail")
	}
}
