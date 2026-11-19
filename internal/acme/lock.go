package acme

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

const (
	// lockPrefix is where lock records live in the certs table.
	lockPrefix = "locks/"
	// lockTTL is how long a lock record is honoured without a refresh. A
	// record older than this is considered stale and may be taken over.
	lockTTL = 5 * time.Minute
	// lockPoll is how long Lock waits between attempts while another
	// holder owns the lock.
	lockPoll = time.Second
)

// heldLock tracks a lock owned by this process.
type heldLock struct {
	cancel context.CancelFunc // stops the refresher
	done   chan struct{}      // closed when the refresher has exited
}

// lockRefresh is how often a held lock's expiry is pushed forward. It is a
// variable so tests can shorten it.
var lockRefresh = lockTTL / 3

func lockKey(name string) string { return lockPrefix + name }

// Lock acquires the named lock, blocking until it is free, stale, or ctx is
// done.
//
// Within one process an in-memory table makes Lock mutually exclusive. On
// disk the lock is a row "locks/<name>" whose value is its expiry
// (unix ms); a holder refreshes it every lockRefresh and a record past its
// expiry (the holder crashed or was partitioned) is taken over.
//
// Limitation: acquisition is check-then-write through the metadata log
// without a fencing token, so two nodes racing for a free or stale lock in
// the same instant may both believe they hold it. certmagic tolerates such
// rare mis-synchronisations (the worst case is a duplicate issuance), and
// Store is last-writer-wins, so no data is corrupted.
func (s *Storage) Lock(ctx context.Context, name string) error {
	key := lockKey(name)
	for {
		if s.tryLocalLock(name) {
			taken, err := s.tryStoreLock(ctx, key)
			if err != nil {
				s.releaseLocal(name)
				return err
			}
			if taken {
				s.startRefresher(name, key)
				return nil
			}
			s.releaseLocal(name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// Unlock releases a lock previously acquired with Lock.
func (s *Storage) Unlock(ctx context.Context, name string) error {
	s.mu.Lock()
	h, ok := s.locks[name]
	delete(s.locks, name)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("acme storage: unlock %q: not held", name)
	}
	h.cancel()
	<-h.done
	if err := s.apply(ctx, &meta.DeleteCert{Key: lockKey(name)}); err != nil {
		return fmt.Errorf("acme storage: unlock %q: %w", name, err)
	}
	return nil
}

// tryLocalLock reserves name in the in-process table. The entry is a
// placeholder until startRefresher replaces it.
func (s *Storage) tryLocalLock(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.locks[name]; held {
		return false
	}
	s.locks[name] = nil
	return true
}

func (s *Storage) releaseLocal(name string) {
	s.mu.Lock()
	delete(s.locks, name)
	s.mu.Unlock()
}

// tryStoreLock writes the lock record if it is absent or stale. It reports
// whether the lock was taken.
func (s *Storage) tryStoreLock(ctx context.Context, key string) (bool, error) {
	now := s.now()
	v, _, err := s.db.GetCert(ctx, key)
	switch {
	case err == nil:
		expiry, perr := strconv.ParseInt(string(v), 10, 64)
		if perr == nil && time.UnixMilli(expiry).After(now) {
			return false, nil // live lock held elsewhere
		}
		// stale or unparsable: fall through and take it over
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("acme storage: lock %q: %w", key, err)
	}
	return true, s.writeLock(ctx, key, now)
}

func (s *Storage) writeLock(ctx context.Context, key string, now time.Time) error {
	expiry := strconv.FormatInt(now.Add(lockTTL).UnixMilli(), 10)
	if err := s.apply(ctx, &meta.PutCert{Key: key, Value: []byte(expiry), Now: now.UnixMilli()}); err != nil {
		return fmt.Errorf("acme storage: lock %q: %w", key, err)
	}
	return nil
}

// startRefresher records the lock as held and keeps its expiry fresh until
// Unlock.
func (s *Storage) startRefresher(name, key string) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &heldLock{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.locks[name] = h
	s.mu.Unlock()
	go func() {
		defer close(h.done)
		t := time.NewTicker(lockRefresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Best effort: a failed refresh only shortens the lease.
				_ = s.writeLock(ctx, key, s.now())
			}
		}
	}()
}
