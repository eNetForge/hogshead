package acme

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

// openDB opens a fresh metadata store in a temp dir.
func openDB(t *testing.T) *meta.DB {
	t.Helper()
	db, err := meta.Open(context.Background(), filepath.Join(t.TempDir(), "meta.db"), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// directPropose applies commands straight to db, as a single node would.
func directPropose(db *meta.DB) Propose {
	return func(_ context.Context, cmd meta.Command) (meta.Result, error) {
		return db.ApplyCommand(0, cmd), nil
	}
}

// clock is a settable test clock, safe for use from the lock refresher.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *clock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

func newStorage(t *testing.T) (*Storage, *meta.DB, *clock) {
	t.Helper()
	db := openDB(t)
	c := &clock{}
	c.set(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	return NewStorage(db, directPropose(db), c.now), db, c
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
