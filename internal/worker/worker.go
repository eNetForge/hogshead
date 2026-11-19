// Package worker runs the background maintenance loops: chunk garbage
// collection, multipart cleanup and lifecycle enforcement.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/store"
)

// Deps are the collaborators the workers need.
type Deps struct {
	DB      *meta.DB
	Propose func(ctx context.Context, cmd meta.Command) (meta.Result, error)
	Chunks  store.ChunkStore
	Local   *store.FS
	Log     *slog.Logger
	Now     func() time.Time
}

// Options tune the loops.
type Options struct {
	GCInterval        time.Duration // default 10m
	GCGrace           time.Duration // unreferenced chunks older than this are removed; default 24h
	OrphanInterval    time.Duration // disk walk for chunks unknown to metadata; default 24h
	UploadInterval    time.Duration // default 1h
	UploadMaxAge      time.Duration // abort uploads older than this; 0 = only via lifecycle rules
	LifecycleInterval time.Duration // default 1h
}

func (o *Options) defaults() {
	if o.GCInterval == 0 {
		o.GCInterval = 10 * time.Minute
	}
	if o.GCGrace == 0 {
		o.GCGrace = 24 * time.Hour
	}
	if o.OrphanInterval == 0 {
		o.OrphanInterval = 24 * time.Hour
	}
	if o.UploadInterval == 0 {
		o.UploadInterval = time.Hour
	}
	if o.LifecycleInterval == 0 {
		o.LifecycleInterval = time.Hour
	}
}

// Runner owns the loops.
type Runner struct {
	d                   Deps
	o                   Options
	defaultUploadMaxAge time.Duration
	wg                  sync.WaitGroup
	stop                context.CancelFunc

	mu    sync.Mutex
	stats Stats
}

// Stats are counters exposed to the admin API.
type Stats struct {
	LastGC          time.Time
	ChunksReleased  int64
	OrphansRemoved  int64
	BytesReclaimed  int64
	LastLifecycle   time.Time
	ObjectsExpired  int64
	VersionsExpired int64
	MarkersRemoved  int64
	UploadsAborted  int64
	LastError       string
}

// New creates a Runner.
func New(d Deps, o Options) *Runner {
	o.defaults()
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Runner{d: d, o: o, defaultUploadMaxAge: o.UploadMaxAge}
}

// Start launches the loops until ctx is cancelled or Stop is called.
func (r *Runner) Start(ctx context.Context) {
	ctx, r.stop = context.WithCancel(ctx)
	r.loop(ctx, r.o.GCInterval, "gc", r.RunGC)
	r.loop(ctx, r.o.OrphanInterval, "orphans", r.RunOrphanScan)
	r.loop(ctx, r.o.UploadInterval, "uploads", r.RunUploadCleanup)
	r.loop(ctx, r.o.LifecycleInterval, "lifecycle", r.RunLifecycle)
}

// applySettings reads the server settings that tune the workers.
func (r *Runner) applySettings(ctx context.Context) {
	if v, err := r.d.DB.GetSetting(ctx, "gc.grace"); err == nil && v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			r.o.GCGrace = d
		}
	}
	if v, err := r.d.DB.GetSetting(ctx, "uploads.max_age"); err == nil {
		if v == "" {
			r.o.UploadMaxAge = r.defaultUploadMaxAge
		} else if d, err := time.ParseDuration(v); err == nil {
			r.o.UploadMaxAge = d
		}
	}
}

// Stop halts the loops and waits for them.
func (r *Runner) Stop() {
	if r.stop != nil {
		r.stop()
	}
	r.wg.Wait()
}

// Stats returns a snapshot of the counters.
func (r *Runner) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

func (r *Runner) loop(ctx context.Context, every time.Duration, name string, fn func(context.Context) error) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// Stagger the first run so startup is not a burst.
		t := time.NewTimer(every / 10)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				r.d.Log.Error("worker failed", "worker", name, "err", err)
				r.setErr(err)
			}
			t.Reset(every)
		}
	}()
}

func (r *Runner) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.LastError = err.Error()
}

func (r *Runner) update(fn func(*Stats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.stats)
}

func (r *Runner) propose(ctx context.Context, cmd meta.Command) error {
	res, err := r.d.Propose(ctx, cmd)
	if err != nil {
		return err
	}
	return res.Error()
}
