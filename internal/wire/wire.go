// Package wire builds the HTTP handlers from a server.Core. It is the only
// place that knows about every layer.
package wire

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/api"
	"github.com/enetforge/hogshead/internal/cellar"
	"github.com/enetforge/hogshead/internal/discovery"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/metrics"
	"github.com/enetforge/hogshead/internal/notify"
	"github.com/enetforge/hogshead/internal/replication"
	"github.com/enetforge/hogshead/internal/server"
	"github.com/enetforge/hogshead/internal/session"
	"github.com/enetforge/hogshead/internal/transfer"
	"github.com/enetforge/hogshead/internal/ui"
	"github.com/enetforge/hogshead/internal/uiauth"
	"github.com/enetforge/hogshead/internal/usage"
	"github.com/enetforge/hogshead/internal/version"
	"github.com/enetforge/hogshead/internal/worker"
)

// Options tune the wiring.
type Options struct {
	ConfigPath string
	UI         http.Handler // optional, served under /_ui/
}

// Build returns the S3 handler (which also serves the admin API with SigV4
// auth and the UI) and the trusted admin handler for the unix socket. It also
// starts the background workers.
func Build(ctx context.Context, core *server.Core, o Options) (http.Handler, http.Handler, error) {
	workers := worker.New(worker.Deps{
		DB: core.DB, Propose: core.ProposeRaw, Chunks: core.Chunks, Local: core.FS,
		Log: core.Log,
	}, worker.Options{})
	workers.Start(ctx)

	m := metrics.New()
	m.Poll(ctx, 15*time.Second, func(ctx context.Context, m *metrics.Metrics) {
		if st, err := core.DB.Stats(ctx); err == nil {
			m.Objects.Set(float64(st.Objects))
			m.Bytes.Set(float64(st.Bytes))
			m.Buckets.Set(float64(st.Buckets))
			m.Uploads.Set(float64(st.Uploads))
			m.ChunksOnDisk.Set(float64(st.Chunks))
		}
		ws := workers.Stats()
		m.WorkerCounter.WithLabelValues("chunks_released").Set(float64(ws.ChunksReleased))
		m.WorkerCounter.WithLabelValues("orphans_removed").Set(float64(ws.OrphansRemoved))
		m.WorkerCounter.WithLabelValues("objects_expired").Set(float64(ws.ObjectsExpired))
		m.WorkerCounter.WithLabelValues("versions_expired").Set(float64(ws.VersionsExpired))
		m.WorkerCounter.WithLabelValues("uploads_aborted").Set(float64(ws.UploadsAborted))
	})

	hooks := notify.New(func(ctx context.Context, name string) (string, string, bool) {
		u, err := core.DB.GetSetting(ctx, notify.SettingPrefix+name)
		if err != nil || u == "" {
			return "", "", false
		}
		secret, _ := core.DB.GetSetting(ctx, notify.SettingPrefix+name+".secret")
		return u, secret, true
	}, core.Log, 2)
	go func() { <-ctx.Done(); hooks.Close() }()
	s3 := api.New(api.Deps{
		DB: core.DB, Propose: core.ProposeRaw, Chunks: core.Chunks, Cfg: core.Cfg, Log: core.Log, Region: "us-east-1",
		Observe: m.Observe, BytesIn: m.BytesIn.Add, BytesOut: m.BytesOut.Add, AuthFailure: m.AuthFailures.Inc,
		Notify: hooks,
	})
	sessions, err := sessionManager(ctx, core)
	if err != nil {
		return nil, nil, err
	}
	serverID, err := ensureServerID(ctx, core)
	if err != nil {
		return nil, nil, err
	}
	name := func() string {
		if n := core.Cfg().Name; n != "" {
			return n
		}
		h, _ := os.Hostname()
		return h
	}
	disco := discovery.New(func() discovery.Beacon {
		cfg := core.Cfg()
		b := discovery.Beacon{ID: serverID, Name: name(), Port: listenPort(cfg.Listen), TLS: cfg.TLS.Mode != "off", Version: version.Version}
		b.Cellar, _ = core.DB.GetSetting(ctx, cellar.KeyID)
		b.CellarName, _ = core.DB.GetSetting(ctx, cellar.KeyName)
		return b
	}, core.Log)
	go disco.Run(ctx, func() bool { return core.Cfg().Discovery.Enabled })
	sessionAuth := &session.Authenticator{Manager: sessions, Lookup: core.DB.GetUser}
	secure := core.Cfg().TLS.Mode != "off"
	s3.SetSession(sessionAuth)
	deps := admin.Deps{
		DB: core.DB, Propose: core.ProposeRaw, Cfg: core.Cfg, ConfigPath: o.ConfigPath, Session: sessionAuth,
		ServerID: func() string { return serverID }, Name: name, Peers: disco.Peers,
		PendingRestart: core.PendingRestart, Version: version.Version, Started: core.Started, Log: core.Log,
		WorkerStats: func() admin.WorkerStats { return workerStats(workers.Stats()) },
	}
	if o.ConfigPath != "" {
		deps.Reload = func() error { return core.Reload(o.ConfigPath) }
	}
	repl := replication.New(replication.Deps{
		DB: core.DB, Propose: core.ProposeRaw, Chunks: core.Chunks, Log: core.Log,
		ServerID: func() string { return serverID },
		WriteAck: func(ctx context.Context) string { v, _ := core.DB.GetSetting(ctx, WriteAckSetting); return v },
	}, 10*time.Second)
	go repl.Run(ctx)
	s3.SetSync(repl)
	s3.SetOnChange(repl.Trigger)
	use := usage.New(core.ProposeRaw, core.Log)
	use.SetAccessLog(core.Cfg().AccessLog)
	core.OnReload(func() { use.SetAccessLog(core.Cfg().AccessLog) })
	go use.Run(ctx, 30*time.Second, func() time.Duration {
		days := 90
		if v, _ := core.DB.GetSetting(ctx, admin.SettingUsageKeep); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				days = n
			}
		}
		return time.Duration(days) * 24 * time.Hour
	})
	s3.SetAccess(use.Handle)
	deps.Replication = repl
	transfers := transfer.New(ctx, transfer.Deps{DB: core.DB, Propose: core.ProposeRaw, Chunks: core.Chunks, Cfg: core.Cfg, Log: core.Log})
	if err := transfers.MarkInterrupted(ctx); err != nil {
		return nil, nil, fmt.Errorf("transfers: %w", err)
	}
	deps.Transfers = transfers
	cel := cellar.New(cellar.Deps{
		DB: core.DB, Propose: core.ProposeRaw, Worker: repl, Version: version.Version, Log: core.Log,
		ServerID: func() string { return serverID }, Name: name,
		Endpoint: func() string {
			cfg := core.Cfg()
			return cellar.SelfEndpoint(cfg.Cellar.Advertise, cfg.Listen, cfg.TLS.Mode != "off")
		},
	})
	go cel.Run(ctx)
	deps.Cellar = cel
	remote := admin.New(deps)
	trusted := deps
	trusted.Trusted = true
	local := admin.New(trusted)

	mux := http.NewServeMux()
	mux.Handle(admin.Prefix+"/", remote)
	mux.HandleFunc("/healthz", health(core))
	mux.HandleFunc("/readyz", readyWith(core, cel.Ready))
	mux.Handle(cellar.PublicPrefix+"/", cel.PublicHandler())
	mux.Handle("/metrics", metrics.TokenHandler(core.Cfg().Metrics.Token, m.Handler()))
	if o.UI != nil {
		mux.Handle(uiauth.Prefix+"/", uiauth.New(uiauth.Deps{DB: core.DB, Propose: core.ProposeRaw, Sessions: sessions, Secure: secure, Log: core.Log}))
		mux.Handle("/_ui/", o.UI)
		mux.HandleFunc("/_ui", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/_ui/", http.StatusFound) })
	}
	mux.Handle("/", s3)
	var public http.Handler = mux
	if core.ACME != nil {
		public = core.ACME.HTTPChallengeHandler(mux)
	}

	socket := http.NewServeMux()
	socket.Handle(admin.Prefix+"/", local)
	socket.HandleFunc("/healthz", health(core))
	socket.Handle("/metrics", m.Handler())
	return public, socket, nil
}

// ensureServerID returns the stable random id of this server, creating it on
// first start. It tags replicated changes and identifies the server on the LAN.
func ensureServerID(ctx context.Context, core *server.Core) (string, error) {
	id, err := core.DB.GetSetting(ctx, ServerIDSetting)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	id = ids.ServerID()
	if _, err := core.Propose(ctx, &meta.SetSetting{Key: ServerIDSetting, Value: id}); err != nil {
		return "", fmt.Errorf("store server id: %w", err)
	}
	return id, nil
}

// ServerIDSetting holds the server's random identity.
const ServerIDSetting = "server.id"

// WriteAckSetting holds the cellar's write acknowledgement level.
const WriteAckSetting = "cellar.write_ack"

// listenPort extracts the port of the first listen address.
func listenPort(listen []string) int {
	if len(listen) == 0 {
		return 0
	}
	_, p, err := net.SplitHostPort(listen[0])
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// sessionManager loads the web UI session secret from settings, creating it
// on first start.
func sessionManager(ctx context.Context, core *server.Core) (*session.Manager, error) {
	hexSecret, err := core.DB.GetSetting(ctx, session.SecretSetting)
	if err != nil {
		return nil, err
	}
	if hexSecret == "" {
		hexSecret = session.NewSecret()
		if _, err := core.Propose(ctx, &meta.SetSetting{Key: session.SecretSetting, Value: hexSecret}); err != nil {
			return nil, fmt.Errorf("store session secret: %w", err)
		}
	}
	secret, err := hex.DecodeString(hexSecret)
	if err != nil {
		return nil, fmt.Errorf("session secret: %w", err)
	}
	return session.New(secret)
}

// Handlers adapts Build to the cmds handler factory signature.
func Handlers(configPath string) func(core *server.Core) (http.Handler, http.Handler, error) {
	return func(core *server.Core) (http.Handler, http.Handler, error) {
		return Build(context.Background(), core, Options{ConfigPath: configPath, UI: ui.Handler()})
	}
}

func health(core *server.Core) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	}
}

func ready(core *server.Core) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if _, err := core.DB.Stats(r.Context()); err != nil {
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	}
}

func workerStats(s worker.Stats) admin.WorkerStats {
	return admin.WorkerStats{
		LastGC: s.LastGC, ChunksReleased: s.ChunksReleased, OrphansRemoved: s.OrphansRemoved, BytesReclaimed: s.BytesReclaimed,
		LastLifecycle: s.LastLifecycle, ObjectsExpired: s.ObjectsExpired, VersionsExpired: s.VersionsExpired,
		MarkersRemoved: s.MarkersRemoved, UploadsAborted: s.UploadsAborted, LastError: s.LastError,
	}
}

// readyWith is ready plus the cellar catch-up gate: a member that still has a
// backlog to receive from its peers answers 503 so load balancers route
// around it while it serves direct requests normally.
func readyWith(core *server.Core, cellarReady func() bool) http.HandlerFunc {
	base := ready(core)
	return func(w http.ResponseWriter, r *http.Request) {
		if !cellarReady() {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("catching up with cellar peers\n"))
			return
		}
		base(w, r)
	}
}
