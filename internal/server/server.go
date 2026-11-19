// Package server composes the node: config, metadata, chunk store and HTTP
// listeners. Handlers for the S3 API and the admin API are plugged in so this
// package has no dependency on them.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enetforge/hogshead/internal/acme"
	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/store"
)

// Core holds the long-lived components every handler needs.
type Core struct {
	Config  atomic.Pointer[config.Config]
	Log     *slog.Logger
	DB      *meta.DB
	Chunks  store.ChunkStore
	FS      *store.FS
	Started time.Time
	// Encrypted reports whether a master key is loaded.
	Encrypted bool
	// ACME is set when tls.mode is acme; handlers wrap themselves with its
	// HTTP-01 challenge responder.
	ACME *acme.Provider

	applyMu        sync.Mutex
	reloadHooks    []func()
	pendingRestart []string
}

// Cfg returns the current configuration snapshot.
func (c *Core) Cfg() config.Config { return *c.Config.Load() }

// Propose applies a metadata command and converts a business error into a Go
// error.
func (c *Core) Propose(ctx context.Context, cmd meta.Command) (meta.Result, error) {
	res, err := c.ProposeRaw(ctx, cmd)
	if err != nil {
		return res, err
	}
	if res.Err != "" {
		return res, res.Error()
	}
	return res, nil
}

// ProposeRaw applies a command and returns the raw Result (business errors
// inside the Result). Commands are applied one at a time.
func (c *Core) ProposeRaw(ctx context.Context, cmd meta.Command) (meta.Result, error) {
	if err := ctx.Err(); err != nil {
		return meta.Result{}, err
	}
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	return c.DB.ApplyCommand(0, cmd), nil
}

// Open creates the Core from a configuration: metadata DB and chunk store.
func Open(ctx context.Context, cfg config.Config, log *slog.Logger) (*Core, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	db, err := meta.Open(ctx, filepath.Join(cfg.DataDir, "meta.db"), log)
	if err != nil {
		return nil, fmt.Errorf("open metadata: %w", err)
	}
	var key []byte
	if kp := cfg.MasterKeyPath(); fileExists(kp) {
		key, err = loadKey(kp)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		log.Info("encryption at rest enabled", "key_file", kp)
	}
	codec, err := store.NewCodec(cfg.Storage.Compress, key)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	fs, err := store.NewFS(filepath.Join(cfg.DataDir, "chunks"), codec)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open chunk store: %w", err)
	}
	c := &Core{Log: log, DB: db, FS: fs, Chunks: fs, Started: time.Now(), Encrypted: key != nil}
	c.Config.Store(&cfg)
	return c, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func loadKey(p string) ([]byte, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others; chmod 600 it", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) < 32 {
		return nil, fmt.Errorf("%s must hold at least 32 bytes", p)
	}
	return b[:32], nil
}

// WriteMasterKey generates a 32-byte key file with 0600 permissions. It
// refuses to overwrite an existing file.
func WriteMasterKey(path string) error {
	if fileExists(path) {
		return fmt.Errorf("%s already exists", path)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, key, 0o600)
}

// Close shuts everything down.
func (c *Core) Close() error {
	if c.DB != nil {
		return c.DB.Close()
	}
	return nil
}

// Listeners runs the HTTP servers for the S3 API and the admin socket.
type Listeners struct {
	core    *Core
	s3      http.Handler
	admin   http.Handler
	tlsConf func() *tls.Config
	servers []*http.Server
	mu      sync.Mutex
}

// NewListeners prepares HTTP servers. tlsConf may return nil for plaintext.
func NewListeners(core *Core, s3, admin http.Handler, tlsConf func() *tls.Config) *Listeners {
	return &Listeners{core: core, s3: s3, admin: admin, tlsConf: tlsConf}
}

// Serve binds all listeners and blocks until ctx is cancelled.
func (l *Listeners) Serve(ctx context.Context) error {
	cfg := l.core.Cfg()
	errCh := make(chan error, len(cfg.Listen)+1)
	var lns []net.Listener

	for _, addr := range cfg.Listen {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		if err != nil {
			l.closeAll(lns)
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		if tc := l.tlsConf(); tc != nil {
			ln = tls.NewListener(ln, tc)
		}
		lns = append(lns, ln)
		srv := &http.Server{
			Handler:           l.s3,
			ReadHeaderTimeout: 30 * time.Second,
			ReadTimeout:       cfg.Limits.RequestTimeout.Std(),
			IdleTimeout:       cfg.Limits.IdleTimeout.Std(),
			MaxHeaderBytes:    cfg.Limits.MaxHeaderBytes,
			ErrorLog:          slog.NewLogLogger(l.core.Log.Handler(), slog.LevelWarn),
		}
		l.add(srv)
		go func(ln net.Listener, srv *http.Server) { errCh <- ignoreClosed(srv.Serve(ln)) }(ln, srv)
		l.core.Log.Info("s3 listening", "addr", ln.Addr().String(), "tls", l.tlsConf() != nil)
	}

	if sock := cfg.Admin.Socket; sock != "" && l.admin != nil {
		_ = os.Remove(sock)
		if err := os.MkdirAll(filepath.Dir(sock), 0o750); err != nil {
			l.closeAll(lns)
			return err
		}
		if len(sock) >= 104 {
			l.closeAll(lns)
			return fmt.Errorf("admin socket path %q is too long (unix sockets allow about 104 bytes); set admin.socket to a shorter path", sock)
		}
		ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", sock)
		if err != nil {
			l.closeAll(lns)
			return fmt.Errorf("admin socket %s: %w", sock, err)
		}
		if err := os.Chmod(sock, 0o660); err != nil { //nolint:gosec // group access to the admin socket is intended
			l.core.Log.Warn("chmod admin socket", "err", err)
		}
		srv := &http.Server{Handler: l.admin, ReadHeaderTimeout: 30 * time.Second}
		l.add(srv)
		go func() { errCh <- ignoreClosed(srv.Serve(ln)) }()
		l.core.Log.Info("admin socket listening", "path", sock)
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			l.shutdown()
			return err
		}
	}
	l.shutdown()
	return nil
}

func (l *Listeners) add(s *http.Server) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.servers = append(l.servers, s)
}

func (l *Listeners) shutdown() {
	l.mu.Lock()
	servers := l.servers
	l.servers = nil
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
}

func (l *Listeners) closeAll(lns []net.Listener) {
	for _, ln := range lns {
		_ = ln.Close()
	}
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
