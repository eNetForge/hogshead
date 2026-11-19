// Package meta is the metadata store: SQLite behind a single deterministic
// Apply function, plus read queries.
package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite" // sqlite driver
)

// DB is the metadata database.
type DB struct {
	path string
	log  *slog.Logger

	mu sync.RWMutex // guards reopen during Restore
	w  *sql.DB      // single writer connection
	r  *sql.DB      // reader pool
}

// Open opens or creates the database at path and migrates the schema.
func Open(ctx context.Context, path string, log *slog.Logger) (*DB, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db := &DB{path: path, log: log}
	if err := db.open(ctx); err != nil {
		return nil, err
	}
	return db, nil
}

func dsn(path string) string {
	return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)"
}

func (db *DB) open(ctx context.Context) error {
	w, err := sql.Open("sqlite", dsn(db.path))
	if err != nil {
		return err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxLifetime(0)
	if err := migrate(ctx, w); err != nil {
		_ = w.Close()
		return err
	}
	r, err := sql.Open("sqlite", dsn(db.path))
	if err != nil {
		_ = w.Close()
		return err
	}
	r.SetMaxOpenConns(16)
	db.w, db.r = w, r
	return nil
}

// Close closes the database.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return errors.Join(db.r.Close(), db.w.Close())
}

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// Reader returns the read pool. Callers must not write through it.
func (db *DB) Reader() *sql.DB {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.r
}

// AppliedIndex returns the last command index applied to this database (0
// when commands are applied without replay tracking).
func (db *DB) AppliedIndex(ctx context.Context) (uint64, error) {
	var s string
	if err := db.Reader().QueryRowContext(ctx, `SELECT value FROM meta WHERE key='applied_index'`).Scan(&s); err != nil {
		return 0, err
	}
	return strconv.ParseUint(s, 10, 64)
}

// Apply decodes and applies one command. index is a monotonic log index; pass 0
// to apply without replay tracking (tests and tooling). Entries with an index
// at or below the recorded applied index are skipped and return an empty
// Result, which makes replay after restart idempotent.
func (db *DB) Apply(index uint64, data []byte) Result {
	cmd, err := Decode(data)
	if err != nil {
		return Result{Err: CodeInternal, Msg: err.Error()}
	}
	return db.ApplyCommand(index, cmd)
}

// ApplyCommand applies an already-decoded command. See Apply.
func (db *DB) ApplyCommand(index uint64, cmd Command) Result {
	db.mu.RLock()
	defer db.mu.RUnlock()
	ctx := context.Background()
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return Result{Err: CodeInternal, Msg: err.Error()}
	}
	defer func() { _ = tx.Rollback() }()

	if index > 0 {
		var cur string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='applied_index'`).Scan(&cur); err != nil {
			return Result{Err: CodeInternal, Msg: err.Error()}
		}
		applied, _ := strconv.ParseUint(cur, 10, 64)
		if index <= applied {
			return Result{}
		}
	}

	// Run the mutation inside a savepoint so a business error leaves the
	// applied index update intact.
	if _, err := tx.ExecContext(ctx, `SAVEPOINT cmd`); err != nil {
		return Result{Err: CodeInternal, Msg: err.Error()}
	}
	a := &applier{ctx: ctx, tx: tx}
	val, aerr := a.apply(cmd)
	res := Result{}
	if aerr != nil {
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT cmd`); err != nil {
			return Result{Err: CodeInternal, Msg: err.Error()}
		}
		var me *Error
		if errors.As(aerr, &me) {
			res.Err, res.Msg = me.Code, me.Msg
		} else {
			db.log.Error("apply failed", "cmd", cmd.name(), "index", index, "err", aerr)
			res.Err, res.Msg = CodeInternal, aerr.Error()
		}
	} else if val != nil {
		b, err := json.Marshal(val)
		if err != nil {
			return Result{Err: CodeInternal, Msg: err.Error()}
		}
		res.Value = b
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT cmd`); err != nil {
		return Result{Err: CodeInternal, Msg: err.Error()}
	}
	if index > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key='applied_index'`, strconv.FormatUint(index, 10)); err != nil {
			return Result{Err: CodeInternal, Msg: err.Error()}
		}
	}
	if err := tx.Commit(); err != nil {
		return Result{Err: CodeInternal, Msg: err.Error()}
	}
	return res
}

// Snapshot writes a consistent copy of the database to dst using VACUUM INTO.
func (db *DB) Snapshot(ctx context.Context, dst string) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	_ = os.Remove(dst)
	// VACUUM INTO cannot run inside a transaction; use the writer so no
	// concurrent Apply interleaves.
	if _, err := db.w.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

// Restore replaces the database with the file at src and reopens it.
func (db *DB) Restore(ctx context.Context, src string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := errors.Join(db.r.Close(), db.w.Close()); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(db.path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(src, db.path); err != nil {
		// Cross-device: fall back to copy.
		if cerr := copyFile(src, db.path); cerr != nil {
			return errors.Join(err, cerr)
		}
	}
	return db.open(ctx)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// applier holds one transaction's state.
type applier struct {
	ctx context.Context
	tx  *sql.Tx
	// origin is the remote server id when the command being applied was
	// replicated from elsewhere (see WithOrigin); recorded on log entries.
	origin string
}

func (a *applier) apply(cmd Command) (any, error) {
	switch c := cmd.(type) {
	case *CreateBucket:
		return nil, a.createBucket(c)
	case *DeleteBucket:
		return nil, a.deleteBucket(c)
	case *SetBucketVersioning:
		return nil, a.setBucketVersioning(c)
	case *SetBucketPolicy:
		return nil, a.setBucketField(c.Name, "policy", c.Policy)
	case *SetBucketCORS:
		return nil, a.setBucketField(c.Name, "cors", c.CORS)
	case *SetBucketLifecycle:
		return nil, a.setBucketField(c.Name, "lifecycle", c.Lifecycle)
	case *PutObjectVersion:
		return a.putObject(c)
	case *DeleteObject:
		return a.deleteObject(c)
	case *DeleteObjects:
		return a.deleteObjects(c)
	case *UpdateObjectMeta:
		return nil, a.updateObjectMeta(c)
	case *CreateUpload:
		return nil, a.createUpload(c)
	case *PutPart:
		return nil, a.putPart(c)
	case *CompleteUpload:
		return a.completeUpload(c)
	case *AbortUpload:
		return nil, a.abortUpload(c)
	case *PurgeCompletedUploads:
		_, err := a.exec(`DELETE FROM uploads WHERE completed = 1 AND created_at < ?`, c.Before)
		return nil, err
	case *CreateUser:
		return nil, a.createUser(c)
	case *UpdateUser:
		return nil, a.updateUser(c)
	case *SetUserPassword:
		return nil, a.setUserPassword(c)
	case *PutTransfer:
		return nil, a.putTransfer(c)
	case *DeleteTransfer:
		return nil, a.deleteTransfer(c)
	case *DeleteUser:
		return nil, a.deleteUser(c)
	case *CreateAccessKey:
		return nil, a.createAccessKey(c)
	case *UpdateAccessKey:
		return nil, a.updateAccessKey(c)
	case *DeleteAccessKey:
		return nil, a.deleteAccessKey(c)
	case *SetSetting:
		return nil, a.setSetting(c)
	case *PutCert:
		return nil, a.putCert(c)
	case *DeleteCert:
		return nil, a.deleteCert(c)
	case *Audit:
		return nil, a.audit(c)
	case *ReleaseChunks:
		return nil, a.releaseChunks(c)
	case *TrimReplicationLog:
		_, err := a.exec(`DELETE FROM replication_log WHERE seq <= ?`, c.Seq)
		return nil, err
	case *Batch:
		return a.batch(c)
	case *WithOrigin:
		return a.withOrigin(c)
	case *AddUsage:
		return nil, a.addUsage(c)
	case *PruneUsage:
		if _, err := a.exec(`DELETE FROM usage WHERE hour < ?`, c.Before); err != nil {
			return nil, err
		}
		_, err := a.exec(`DELETE FROM request_errors WHERE at < ?`, c.Before*1000)
		return nil, err
	case *AddErrors:
		for _, r := range c.Rows {
			if _, err := a.exec(`INSERT INTO request_errors(at, data) VALUES(?,?)`, r.At, r.Data); err != nil {
				return nil, err
			}
		}
		if c.Keep > 0 {
			if _, err := a.exec(`DELETE FROM request_errors WHERE id <= (SELECT MAX(id) FROM request_errors) - ?`, c.Keep); err != nil {
				return nil, err
			}
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unhandled command %T", cmd)
	}
}

// withOrigin applies the wrapped command with its origin recorded.
func (a *applier) withOrigin(c *WithOrigin) (any, error) {
	inner, err := Decode(c.Command)
	if err != nil {
		return nil, err
	}
	saved := a.origin
	a.origin = c.Origin
	defer func() { a.origin = saved }()
	return a.apply(inner)
}

func (a *applier) batch(c *Batch) (any, error) {
	out := make([]Result, 0, len(c.Commands))
	for _, raw := range c.Commands {
		cmd, err := Decode(raw)
		if err != nil {
			return nil, err
		}
		v, err := a.apply(cmd)
		if err != nil {
			return nil, err
		}
		r := Result{}
		if v != nil {
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			r.Value = b
		}
		out = append(out, r)
	}
	return out, nil
}

func (a *applier) exec(q string, args ...any) (sql.Result, error) {
	return a.tx.ExecContext(a.ctx, q, args...)
}

func (a *applier) nextSeq() (int64, error) {
	var s string
	if err := a.tx.QueryRowContext(a.ctx, `SELECT value FROM meta WHERE key='seq'`).Scan(&s); err != nil {
		return 0, err
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	n++
	if _, err := a.exec(`UPDATE meta SET value=? WHERE key='seq'`, strconv.FormatInt(n, 10)); err != nil {
		return 0, err
	}
	return n, nil
}

// ---- chunk reference counting ----

func (a *applier) addRefs(chunks []ChunkRef) error {
	for _, c := range chunks {
		if _, err := a.exec(`INSERT INTO chunks(hash, size, refs, unref_at) VALUES(?,?,1,0)
			ON CONFLICT(hash) DO UPDATE SET refs = refs + 1, unref_at = 0`, c.Hash, c.Size); err != nil {
			return err
		}
	}
	return nil
}

func (a *applier) releaseRefs(chunks []ChunkRef, now int64) error {
	for _, c := range chunks {
		if _, err := a.exec(`UPDATE chunks SET refs = MAX(refs - 1, 0),
			unref_at = CASE WHEN refs - 1 <= 0 THEN ? ELSE unref_at END WHERE hash = ?`, now, c.Hash); err != nil {
			return err
		}
	}
	return nil
}

func (a *applier) releaseChunks(c *ReleaseChunks) error {
	for _, h := range c.Hashes {
		if _, err := a.exec(`DELETE FROM chunks WHERE hash = ? AND refs = 0`, h); err != nil {
			return err
		}
	}
	return nil
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func decodeChunks(s string) []ChunkRef {
	var out []ChunkRef
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func encodeJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
