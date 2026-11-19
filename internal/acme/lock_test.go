package acme

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

func lockExpiry(t *testing.T, db *meta.DB, name string) time.Time {
	t.Helper()
	v, _, err := db.GetCert(context.Background(), lockKey(name))
	if err != nil {
		t.Fatalf("lock record %q: %v", name, err)
	}
	ms, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil {
		t.Fatalf("lock record %q value %q: %v", name, v, err)
	}
	return time.UnixMilli(ms)
}

func TestLockUnlock(t *testing.T) {
	s, db, c := newStorage(t)
	ctx := context.Background()

	if err := s.Lock(ctx, "issue_cert_example.com"); err != nil {
		t.Fatal(err)
	}
	if got, want := lockExpiry(t, db, "issue_cert_example.com"), c.now().Add(lockTTL); !got.Equal(want) {
		t.Fatalf("expiry: got %v want %v", got, want)
	}

	// A second acquisition in the same process blocks until ctx expires.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := s.Lock(short, "issue_cert_example.com"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended Lock: want DeadlineExceeded, got %v", err)
	}

	if err := s.Unlock(ctx, "issue_cert_example.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.GetCert(ctx, lockKey("issue_cert_example.com")); err == nil {
		t.Fatal("lock record should be gone after Unlock")
	}
	// Re-acquire after release works immediately.
	if err := s.Lock(ctx, "issue_cert_example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.Unlock(ctx, "issue_cert_example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestLockBlocksThenAcquiresAfterUnlock(t *testing.T) {
	s, _, _ := newStorage(t)
	ctx := context.Background()
	if err := s.Lock(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- s.Lock(ctx, "x") }()
	time.Sleep(20 * time.Millisecond)
	if err := s.Unlock(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired the lock")
	}
	if err := s.Unlock(ctx, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestUnlockNotHeld(t *testing.T) {
	s, _, _ := newStorage(t)
	if err := s.Unlock(context.Background(), "never"); err == nil {
		t.Fatal("want error unlocking a lock that is not held")
	}
}

// Two Storage values over the same DB stand in for two cluster nodes.
func TestLockAcrossNodesAndStaleTakeover(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cA, cB := &clock{}, &clock{}
	cA.set(base)
	cB.set(base)
	nodeA := NewStorage(db, directPropose(db), cA.now)
	nodeB := NewStorage(db, directPropose(db), cB.now)

	if err := nodeA.Lock(ctx, "renew"); err != nil {
		t.Fatal(err)
	}

	// B sees a live lock and must wait.
	cB.set(base.Add(lockTTL - time.Second))
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := nodeB.Lock(short, "renew"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live lock on other node: want DeadlineExceeded, got %v", err)
	}

	// A "crashes": its record is never refreshed or removed. Once the TTL
	// has passed B takes the lock over.
	cB.set(base.Add(lockTTL + time.Second))
	if err := nodeB.Lock(ctx, "renew"); err != nil {
		t.Fatalf("stale takeover: %v", err)
	}
	if got, want := lockExpiry(t, db, "renew"), cB.now().Add(lockTTL); !got.Equal(want) {
		t.Fatalf("expiry after takeover: got %v want %v", got, want)
	}
	if err := nodeB.Unlock(ctx, "renew"); err != nil {
		t.Fatal(err)
	}
	// A still believes it holds the lock; its Unlock succeeds (record gone).
	if err := nodeA.Unlock(ctx, "renew"); err != nil {
		t.Fatal(err)
	}
}

func TestLockTakesOverCorruptRecord(t *testing.T) {
	s, db, _ := newStorage(t)
	ctx := context.Background()
	if r := db.ApplyCommand(0, &meta.PutCert{Key: lockKey("bad"), Value: []byte("not-a-number"), Now: 1}); r.Err != "" {
		t.Fatal(r.Msg)
	}
	if err := s.Lock(ctx, "bad"); err != nil {
		t.Fatalf("corrupt record should be taken over: %v", err)
	}
	if err := s.Unlock(ctx, "bad"); err != nil {
		t.Fatal(err)
	}
}

func TestLockRefreshesExpiry(t *testing.T) {
	old := lockRefresh
	lockRefresh = 5 * time.Millisecond
	t.Cleanup(func() { lockRefresh = old })

	s, db, c := newStorage(t)
	ctx := context.Background()
	if err := s.Lock(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	first := lockExpiry(t, db, "r")
	c.advance(time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for lockExpiry(t, db, "r").Equal(first) {
		if time.Now().After(deadline) {
			t.Fatal("lock expiry was never refreshed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Unlock(ctx, "r"); err != nil {
		t.Fatal(err)
	}
}

func TestLockPropagatesStoreErrors(t *testing.T) {
	db := openDB(t)
	s := NewStorage(db, func(context.Context, meta.Command) (meta.Result, error) {
		return meta.Result{}, errors.New("not leader")
	}, nil)
	if err := s.Lock(context.Background(), "x"); err == nil {
		t.Fatal("want error when the lock record cannot be written")
	}
	// The in-process reservation must have been released.
	s.mu.Lock()
	_, held := s.locks["x"]
	s.mu.Unlock()
	if held {
		t.Fatal("local reservation leaked after failed Lock")
	}
}
