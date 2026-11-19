package server

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/enetforge/hogshead/internal/config"
)

var reloadMu sync.Mutex

// OnReload registers fn to run after a successful config reload.
func (c *Core) OnReload(fn func()) {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	c.reloadHooks = append(c.reloadHooks, fn)
}

// PendingRestart lists settings changed on disk that need a restart.
func (c *Core) PendingRestart() []string {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	return append([]string(nil), c.pendingRestart...)
}

// Reload re-reads the config file and applies live settings.
func (c *Core) Reload(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	old := c.Cfg()
	restart := config.RestartRequired(old, cfg)
	// Keep restart-only values from the running config so the live snapshot
	// describes what is actually in effect.
	cfg.DataDir, cfg.Listen, cfg.Admin.Socket = old.DataDir, old.Listen, old.Admin.Socket
	cfg.TLS.Mode, cfg.Storage.KeyFile = old.TLS.Mode, old.Storage.KeyFile
	c.Config.Store(&cfg)
	reloadMu.Lock()
	c.pendingRestart = restart
	hooks := append([]func(){}, c.reloadHooks...)
	reloadMu.Unlock()
	for _, h := range hooks {
		h()
	}
	if len(restart) > 0 {
		c.Log.Warn("config changes require a restart", "settings", restart)
	}
	c.Log.Info("config reloaded", "path", path)
	return nil
}

// WatchConfig reloads on SIGHUP and when the file changes on disk (polled
// every two seconds; fsnotify is avoided to keep the binary dependency-free).
func WatchConfig(ctx context.Context, path string, c *Core) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	var lastMod time.Time
	if st, err := os.Stat(path); err == nil {
		lastMod = st.ModTime()
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if err := c.Reload(path); err != nil {
				c.Log.Error("reload failed", "err", err)
			}
		case <-tick.C:
			st, err := os.Stat(path)
			if err != nil || st.ModTime().Equal(lastMod) {
				continue
			}
			lastMod = st.ModTime()
			if err := c.Reload(path); err != nil {
				c.Log.Error("reload failed", "err", err)
			}
		}
	}
}
