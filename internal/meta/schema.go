package meta

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; the index+1 is the schema version.
var migrations = []string{
	`
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO meta(key, value) VALUES ('applied_index', '0'), ('seq', '0');

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE users (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  role       TEXT NOT NULL,
  buckets    TEXT NOT NULL DEFAULT '[]',
  disabled   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE access_keys (
  access_key TEXT PRIMARY KEY,
  secret     TEXT NOT NULL,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  label      TEXT NOT NULL DEFAULT '',
  disabled   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX access_keys_user ON access_keys(user_id);

CREATE TABLE buckets (
  name              TEXT PRIMARY KEY,
  owner             TEXT NOT NULL,
  created_at        INTEGER NOT NULL,
  versioning        TEXT NOT NULL DEFAULT '',
  policy            TEXT NOT NULL DEFAULT '',
  cors              TEXT NOT NULL DEFAULT '',
  lifecycle         TEXT NOT NULL DEFAULT '',
  write_consistency TEXT NOT NULL DEFAULT '',
  partition_reads   TEXT NOT NULL DEFAULT '',
  object_lock       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE versions (
  bucket        TEXT NOT NULL,
  key           TEXT NOT NULL,
  version_id    TEXT NOT NULL,
  seq           INTEGER NOT NULL,
  is_latest     INTEGER NOT NULL,
  delete_marker INTEGER NOT NULL DEFAULT 0,
  size          INTEGER NOT NULL DEFAULT 0,
  etag          TEXT NOT NULL DEFAULT '',
  content_type  TEXT NOT NULL DEFAULT '',
  metadata      TEXT NOT NULL DEFAULT '{}',
  chunks        TEXT NOT NULL DEFAULT '[]',
  checksums     TEXT NOT NULL DEFAULT '{}',
  storage_class TEXT NOT NULL DEFAULT '',
  owner         TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (bucket, key, version_id)
) WITHOUT ROWID;
CREATE INDEX versions_latest ON versions(bucket, key) WHERE is_latest = 1;
CREATE INDEX versions_seq ON versions(bucket, key, seq DESC);

CREATE TABLE uploads (
  upload_id     TEXT PRIMARY KEY,
  bucket        TEXT NOT NULL,
  key           TEXT NOT NULL,
  owner         TEXT NOT NULL DEFAULT '',
  content_type  TEXT NOT NULL DEFAULT '',
  metadata      TEXT NOT NULL DEFAULT '{}',
  checksum_algo TEXT NOT NULL DEFAULT '',
  checksum_type TEXT NOT NULL DEFAULT '',
  storage_class TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);
CREATE INDEX uploads_bucket ON uploads(bucket, key, upload_id);

CREATE TABLE parts (
  upload_id   TEXT NOT NULL REFERENCES uploads(upload_id) ON DELETE CASCADE,
  part_number INTEGER NOT NULL,
  size        INTEGER NOT NULL,
  etag        TEXT NOT NULL,
  chunks      TEXT NOT NULL DEFAULT '[]',
  checksums   TEXT NOT NULL DEFAULT '{}',
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (upload_id, part_number)
) WITHOUT ROWID;

CREATE TABLE chunks (
  hash     TEXT PRIMARY KEY,
  size     INTEGER NOT NULL,
  refs     INTEGER NOT NULL DEFAULT 0,
  unref_at INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX chunks_unref ON chunks(unref_at) WHERE refs = 0;

CREATE TABLE nodes (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  addr      TEXT NOT NULL,
  zone      TEXT NOT NULL,
  voter     INTEGER NOT NULL DEFAULT 0,
  draining  INTEGER NOT NULL DEFAULT 0,
  joined_at INTEGER NOT NULL
);

CREATE TABLE certs (
  key      TEXT PRIMARY KEY,
  value    BLOB NOT NULL,
  modified INTEGER NOT NULL
);

CREATE TABLE audit (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  at     INTEGER NOT NULL,
  actor  TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_at ON audit(at);
`,
	`
ALTER TABLE uploads ADD COLUMN completed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE uploads ADD COLUMN result TEXT NOT NULL DEFAULT '';
`,
	`
CREATE TABLE replication_log (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  bucket     TEXT NOT NULL,
  key        TEXT NOT NULL,
  version_id TEXT NOT NULL,
  op         TEXT NOT NULL,
  at         INTEGER NOT NULL
);
CREATE INDEX replication_log_bucket ON replication_log(bucket, seq);
`,
	`
DROP TABLE IF EXISTS nodes;
`,
	// 5: optional password login for the web UI.
	`ALTER TABLE users ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';`,
	// 6: transfer (import/export) job records.
	`CREATE TABLE transfers (
  id      TEXT PRIMARY KEY,
  created INTEGER NOT NULL,
  data    TEXT NOT NULL
);`,
	// 7: where a change came from, so replicated writes are not forwarded again.
	`ALTER TABLE replication_log ADD COLUMN origin TEXT NOT NULL DEFAULT '';`,
	// 8: hourly S3 usage counters per bucket, user, access key and class.
	`CREATE TABLE usage (
  hour       INTEGER NOT NULL,
  bucket     TEXT NOT NULL,
  user       TEXT NOT NULL,
  access_key TEXT NOT NULL,
  class      TEXT NOT NULL,
  requests   INTEGER NOT NULL DEFAULT 0,
  errors     INTEGER NOT NULL DEFAULT 0,
  bytes_in   INTEGER NOT NULL DEFAULT 0,
  bytes_out  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (hour, bucket, user, access_key, class)
);
CREATE INDEX usage_hour ON usage(hour);`,
	// 9: recent failed S3 requests, for the Errors page.
	`CREATE TABLE request_errors (
  id   INTEGER PRIMARY KEY AUTOINCREMENT,
  at   INTEGER NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX request_errors_at ON request_errors(at);`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
