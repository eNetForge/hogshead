package acme

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/enetforge/hogshead/internal/meta"
)

// Propose submits a metadata command and returns its result.
// Business errors are carried inside Result.Err.
type Propose func(context.Context, meta.Command) (meta.Result, error)

// Storage implements certmagic.Storage on top of the metadata store so a
// restarted server keeps its certificates and ACME account.
//
// Keys are stored verbatim in the certs table; "directories" are implicit
// prefixes separated by "/". Locks are rows under "locks/" whose value is
// the lock's expiry in unix milliseconds (see lock.go).
type Storage struct {
	db      *meta.DB
	propose Propose
	now     func() time.Time

	mu    sync.Mutex
	locks map[string]*heldLock // locks held by this process, by name
}

var _ certmagic.Storage = (*Storage)(nil)

// NewStorage builds a Storage over db. propose applies mutations to the
// metadata store. now is the clock used for timestamps and lock expiry; nil
// means time.Now.
func NewStorage(db *meta.DB, propose Propose, now func() time.Time) *Storage {
	if now == nil {
		now = time.Now
	}
	return &Storage{db: db, propose: propose, now: now, locks: map[string]*heldLock{}}
}

func (s *Storage) apply(ctx context.Context, cmd meta.Command) error {
	res, err := s.propose(ctx, cmd)
	if err != nil {
		return err
	}
	return res.Error()
}

// Store puts value at key, creating or overwriting it.
func (s *Storage) Store(ctx context.Context, key string, value []byte) error {
	if key == "" {
		return errors.New("acme storage: empty key")
	}
	if err := s.apply(ctx, &meta.PutCert{Key: key, Value: value, Now: s.now().UnixMilli()}); err != nil {
		return fmt.Errorf("acme storage: store %q: %w", key, err)
	}
	return nil
}

// Load returns the value at key, or fs.ErrNotExist.
func (s *Storage) Load(ctx context.Context, key string) ([]byte, error) {
	v, _, err := s.db.GetCert(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("acme storage: load %q: %w", key, err)
	}
	return v, nil
}

// Delete removes key and, when key is a directory, everything below it.
// Deleting a missing key is not an error.
func (s *Storage) Delete(ctx context.Context, key string) error {
	if err := s.apply(ctx, &meta.DeleteCert{Key: key, Prefix: true}); err != nil {
		return fmt.Errorf("acme storage: delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether key exists as a file or a directory.
func (s *Storage) Exists(ctx context.Context, key string) bool {
	_, err := s.Stat(ctx, key)
	return err == nil
}

// List returns the keys below prefix. With recursive=false only direct
// children are returned (directories appear once, without a trailing "/").
// An empty result is reported as fs.ErrNotExist, matching certmagic's
// FileStorage.
func (s *Storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	q := strings.TrimSuffix(prefix, "/")
	if q != "" {
		q += "/"
	}
	raw, err := s.db.ListCerts(ctx, q, recursive)
	if err != nil {
		return nil, fmt.Errorf("acme storage: list %q: %w", prefix, err)
	}
	if len(raw) == 0 {
		return nil, fs.ErrNotExist
	}
	// With an empty prefix the store reports collapsed directories as
	// "/name"; normalise and keep the output sorted so callers see a stable
	// order regardless of how directories were collapsed.
	keys := make([]string, len(raw))
	for i, k := range raw {
		keys[i] = strings.TrimPrefix(k, "/")
	}
	sort.Strings(keys)
	return keys, nil
}

// Stat returns information about key. A key that is only a prefix of other
// keys is reported as a directory.
func (s *Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	v, mod, err := s.db.GetCert(ctx, key)
	switch {
	case err == nil:
		return certmagic.KeyInfo{
			Key:        key,
			Modified:   time.UnixMilli(mod),
			Size:       int64(len(v)),
			IsTerminal: true,
		}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return certmagic.KeyInfo{}, fmt.Errorf("acme storage: stat %q: %w", key, err)
	}
	if key == "" {
		return certmagic.KeyInfo{}, fs.ErrNotExist
	}
	children, err := s.db.ListCerts(ctx, strings.TrimSuffix(key, "/")+"/", true)
	if err != nil {
		return certmagic.KeyInfo{}, fmt.Errorf("acme storage: stat %q: %w", key, err)
	}
	if len(children) == 0 {
		return certmagic.KeyInfo{}, fs.ErrNotExist
	}
	return certmagic.KeyInfo{Key: key, IsTerminal: false}, nil
}
