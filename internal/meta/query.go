package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// likeEscape escapes LIKE metacharacters using backslash.
func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// prefixEnd returns the smallest string greater than every string with the
// given prefix, or "" if no such string exists.
func prefixEnd(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

// ---- buckets ----

func scanBucket(row interface{ Scan(...any) error }) (*Bucket, error) {
	var b Bucket
	var created int64
	var lock int
	err := row.Scan(&b.Name, &b.Owner, &created, &b.Versioning, &b.Policy, &b.CORS, &b.Lifecycle,
		&b.WriteConsistency, &b.PartitionReads, &lock)
	if err != nil {
		return nil, err
	}
	b.CreatedAt = msToTime(created)
	b.ObjectLock = lock == 1
	return &b, nil
}

const bucketCols = `name, owner, created_at, versioning, policy, cors, lifecycle, write_consistency, partition_reads, object_lock`

// GetBucket returns a bucket or ErrNoSuchBucket.
func (db *DB) GetBucket(ctx context.Context, name string) (*Bucket, error) {
	b, err := scanBucket(db.Reader().QueryRowContext(ctx, `SELECT `+bucketCols+` FROM buckets WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchBucket
	}
	return b, err
}

// ListBuckets returns all buckets, optionally filtered by owner.
func (db *DB) ListBuckets(ctx context.Context, owner string) ([]Bucket, error) {
	q := `SELECT ` + bucketCols + ` FROM buckets`
	var args []any
	if owner != "" {
		q += ` WHERE owner = ?`
		args = append(args, owner)
	}
	rows, err := db.Reader().QueryContext(ctx, q+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Bucket
	for rows.Next() {
		b, err := scanBucket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// ---- objects ----

const versionCols = `bucket, key, version_id, seq, is_latest, delete_marker, size, etag, content_type, metadata, chunks, checksums, storage_class, owner, created_at`

func scanVersion(row interface{ Scan(...any) error }) (*ObjectVersion, error) {
	var v ObjectVersion
	var latest, dm int
	var meta, chunks, cks string
	var created int64
	if err := row.Scan(&v.Bucket, &v.Key, &v.VersionID, &v.Seq, &latest, &dm, &v.Size, &v.ETag, &v.ContentType,
		&meta, &chunks, &cks, &v.StorageClass, &v.Owner, &created); err != nil {
		return nil, err
	}
	v.IsLatest, v.DeleteMarker = latest == 1, dm == 1
	v.Created = msToTime(created)
	_ = json.Unmarshal([]byte(meta), &v.Metadata)
	v.Chunks = decodeChunks(chunks)
	_ = json.Unmarshal([]byte(cks), &v.Checksums)
	return &v, nil
}

// GetObject returns the latest version of key, or the specific version when
// versionID is non-empty. A latest delete marker is returned as such; callers
// decide how to present it.
func (db *DB) GetObject(ctx context.Context, bucket, key, versionID string) (*ObjectVersion, error) {
	var row *sql.Row
	if versionID == "" {
		row = db.Reader().QueryRowContext(ctx, `SELECT `+versionCols+` FROM versions WHERE bucket=? AND key=? AND is_latest=1`, bucket, key)
	} else {
		row = db.Reader().QueryRowContext(ctx, `SELECT `+versionCols+` FROM versions WHERE bucket=? AND key=? AND version_id=?`, bucket, key, versionID)
	}
	v, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		if versionID != "" {
			return nil, ErrNoSuchVersion
		}
		return nil, ErrNoSuchKey
	}
	return v, err
}

// ListOpts controls ListObjects.
type ListOpts struct {
	Prefix    string
	Delimiter string
	// StartAfter is exclusive. When Delimiter is set and StartAfter ends
	// with it, every key under that prefix is skipped, which is how a
	// NextCursor equal to a common prefix continues correctly.
	StartAfter string
	Max        int
}

// ListResult is the outcome of ListObjects.
type ListResult struct {
	Objects        []ObjectVersion
	CommonPrefixes []string
	IsTruncated    bool
	NextCursor     string // pass as StartAfter to continue
}

// bound is a lower bound on (key, seq) for listing queries.
type bound struct {
	key       string
	inclusive bool  // key >= bound.key instead of key > bound.key
	seq       int64 // when >= 0, rows with key == bound.key and seq < seq also match
}

func (b bound) where(args *[]any) string {
	if b.seq >= 0 {
		*args = append(*args, b.key, b.key, b.seq)
		return `(key > ? OR (key = ? AND seq < ?))`
	}
	*args = append(*args, b.key)
	if b.inclusive {
		return `key >= ?`
	}
	return `key > ?`
}

// afterPrefix returns a bound that skips every key starting with p, or ok=false
// when no key can follow.
func afterPrefix(p string) (bound, bool) {
	e := prefixEnd(p)
	if e == "" {
		return bound{}, false
	}
	return bound{key: e, inclusive: true, seq: -1}, true
}

func startBound(prefix, startAfter, delimiter string) (bound, bool) {
	if startAfter < prefix {
		return bound{key: prefix, inclusive: true, seq: -1}, true
	}
	if delimiter != "" && startAfter != "" && strings.HasSuffix(startAfter, delimiter) {
		return afterPrefix(startAfter)
	}
	return bound{key: startAfter, seq: -1}, true
}

// commonPrefix returns the delimiter-rolled prefix of key, if any.
func commonPrefix(key, prefix, delimiter string) (string, bool) {
	if delimiter == "" {
		return "", false
	}
	rest := strings.TrimPrefix(key, prefix)
	i := strings.Index(rest, delimiter)
	if i < 0 {
		return "", false
	}
	return prefix + rest[:i+len(delimiter)], true
}

func (db *DB) queryVersions(ctx context.Context, bucket string, latestOnly bool, b bound, end string, limit int) ([]*ObjectVersion, error) {
	var args []any
	args = append(args, bucket)
	q := `SELECT ` + versionCols + ` FROM versions WHERE bucket=?`
	if latestOnly {
		q += ` AND is_latest=1 AND delete_marker=0`
	}
	q += ` AND ` + b.where(&args)
	if end != "" {
		q += ` AND key < ?`
		args = append(args, end)
	}
	q += ` ORDER BY key, seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var got []*ObjectVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		got = append(got, v)
	}
	return got, rows.Err()
}

func batchSize(max, count int) int {
	n := max - count + 1
	if n > 1001 {
		n = 1001
	}
	return n
}

// ListObjects lists latest, non-deleted versions with S3 prefix/delimiter
// semantics.
func (db *DB) ListObjects(ctx context.Context, bucket string, o ListOpts) (*ListResult, error) {
	if o.Max <= 0 {
		o.Max = 1000
	}
	res := &ListResult{}
	b, ok := startBound(o.Prefix, o.StartAfter, o.Delimiter)
	if !ok {
		return res, nil
	}
	end := prefixEnd(o.Prefix)
	count := 0
	seen := map[string]bool{}
	for {
		n := batchSize(o.Max, count)
		got, err := db.queryVersions(ctx, bucket, true, b, end, n)
		if err != nil {
			return nil, err
		}
		if len(got) == 0 {
			return res, nil
		}
		restart := false
		for _, v := range got {
			if count >= o.Max {
				res.IsTruncated = true
				return res, nil
			}
			if cp, isCP := commonPrefix(v.Key, o.Prefix, o.Delimiter); isCP {
				if !seen[cp] {
					seen[cp] = true
					res.CommonPrefixes = append(res.CommonPrefixes, cp)
					count++
					res.NextCursor = cp
				}
				b, ok = afterPrefix(cp)
				if !ok {
					return res, nil
				}
				restart = true
				break
			}
			res.Objects = append(res.Objects, *v)
			count++
			b = bound{key: v.Key, seq: -1}
			res.NextCursor = v.Key
		}
		if !restart && len(got) < n {
			return res, nil
		}
	}
}

// VersionListOpts controls ListVersions.
type VersionListOpts struct {
	Prefix          string
	Delimiter       string
	KeyMarker       string
	VersionIDMarker string
	Max             int
}

// VersionListResult is the outcome of ListVersions.
type VersionListResult struct {
	Versions            []ObjectVersion
	CommonPrefixes      []string
	IsTruncated         bool
	NextKeyMarker       string
	NextVersionIDMarker string
}

// ListVersions lists all versions including delete markers, ordered by key
// then newest first.
func (db *DB) ListVersions(ctx context.Context, bucket string, o VersionListOpts) (*VersionListResult, error) {
	if o.Max <= 0 {
		o.Max = 1000
	}
	res := &VersionListResult{}
	b, ok := startBound(o.Prefix, o.KeyMarker, o.Delimiter)
	if !ok {
		return res, nil
	}
	if o.KeyMarker != "" && o.VersionIDMarker != "" && o.KeyMarker >= o.Prefix {
		var seq int64
		err := db.Reader().QueryRowContext(ctx, `SELECT seq FROM versions WHERE bucket=? AND key=? AND version_id=?`,
			bucket, o.KeyMarker, o.VersionIDMarker).Scan(&seq)
		if err == nil {
			b = bound{key: o.KeyMarker, seq: seq}
		}
	}
	end := prefixEnd(o.Prefix)
	count := 0
	seen := map[string]bool{}
	for {
		n := batchSize(o.Max, count)
		got, err := db.queryVersions(ctx, bucket, false, b, end, n)
		if err != nil {
			return nil, err
		}
		if len(got) == 0 {
			return res, nil
		}
		restart := false
		for _, v := range got {
			if count >= o.Max {
				res.IsTruncated = true
				return res, nil
			}
			if cp, isCP := commonPrefix(v.Key, o.Prefix, o.Delimiter); isCP {
				if !seen[cp] {
					seen[cp] = true
					res.CommonPrefixes = append(res.CommonPrefixes, cp)
					count++
					res.NextKeyMarker, res.NextVersionIDMarker = cp, ""
				}
				b, ok = afterPrefix(cp)
				if !ok {
					return res, nil
				}
				restart = true
				break
			}
			res.Versions = append(res.Versions, *v)
			count++
			b = bound{key: v.Key, seq: v.Seq}
			res.NextKeyMarker, res.NextVersionIDMarker = v.Key, v.VersionID
		}
		if !restart && len(got) < n {
			return res, nil
		}
	}
}

// ---- multipart ----

// GetUpload returns an upload or ErrNoSuchUpload.
func (db *DB) GetUpload(ctx context.Context, id string) (*Upload, error) {
	var u Upload
	var meta string
	var created int64
	err := db.Reader().QueryRowContext(ctx, `SELECT upload_id, bucket, key, owner, content_type, metadata, checksum_algo, checksum_type, storage_class, created_at FROM uploads WHERE upload_id=? AND completed = 0`, id).
		Scan(&u.ID, &u.Bucket, &u.Key, &u.Owner, &u.ContentType, &meta, &u.ChecksumAlgo, &u.ChecksumType, &u.StorageClass, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchUpload
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(meta), &u.Metadata)
	u.Created = msToTime(created)
	return &u, nil
}

// ListParts returns parts after marker, up to max, plus whether more exist.
func (db *DB) ListParts(ctx context.Context, id string, marker, max int) ([]Part, bool, error) {
	if max <= 0 {
		max = 1000
	}
	rows, err := db.Reader().QueryContext(ctx, `SELECT part_number, size, etag, chunks, checksums, created_at FROM parts WHERE upload_id=? AND part_number > ? ORDER BY part_number LIMIT ?`, id, marker, max+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var out []Part
	for rows.Next() {
		var p Part
		var ch, cks string
		var created int64
		if err := rows.Scan(&p.Number, &p.Size, &p.ETag, &ch, &cks, &created); err != nil {
			return nil, false, err
		}
		p.UploadID = id
		p.Chunks = decodeChunks(ch)
		_ = json.Unmarshal([]byte(cks), &p.Checksums)
		p.Created = msToTime(created)
		out = append(out, p)
	}
	more := len(out) > max
	if more {
		out = out[:max]
	}
	return out, more, rows.Err()
}

// UploadListOpts controls ListUploads.
type UploadListOpts struct {
	Prefix         string
	Delimiter      string
	KeyMarker      string
	UploadIDMarker string
	Max            int
}

// UploadListResult is the outcome of ListUploads.
type UploadListResult struct {
	Uploads            []Upload
	CommonPrefixes     []string
	IsTruncated        bool
	NextKeyMarker      string
	NextUploadIDMarker string
}

// ListUploads lists in-progress uploads for a bucket.
func (db *DB) ListUploads(ctx context.Context, bucket string, o UploadListOpts) (*UploadListResult, error) {
	if o.Max <= 0 {
		o.Max = 1000
	}
	q := `SELECT upload_id, bucket, key, owner, content_type, metadata, checksum_algo, checksum_type, storage_class, created_at FROM uploads WHERE bucket=? AND completed = 0`
	args := []any{bucket}
	if o.Prefix != "" {
		q += ` AND key >= ?`
		args = append(args, o.Prefix)
		if e := prefixEnd(o.Prefix); e != "" {
			q += ` AND key < ?`
			args = append(args, e)
		}
	}
	if o.KeyMarker != "" {
		if o.UploadIDMarker != "" {
			q += ` AND (key > ? OR (key = ? AND upload_id > ?))`
			args = append(args, o.KeyMarker, o.KeyMarker, o.UploadIDMarker)
		} else {
			q += ` AND key > ?`
			args = append(args, o.KeyMarker)
		}
	}
	q += ` ORDER BY key, upload_id`
	rows, err := db.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	res := &UploadListResult{}
	seen := map[string]bool{}
	count := 0
	for rows.Next() {
		var u Upload
		var meta string
		var created int64
		if err := rows.Scan(&u.ID, &u.Bucket, &u.Key, &u.Owner, &u.ContentType, &meta, &u.ChecksumAlgo, &u.ChecksumType, &u.StorageClass, &created); err != nil {
			return nil, err
		}
		if count >= o.Max {
			res.IsTruncated = true
			break
		}
		if o.Delimiter != "" {
			rest := strings.TrimPrefix(u.Key, o.Prefix)
			if i := strings.Index(rest, o.Delimiter); i >= 0 {
				cp := o.Prefix + rest[:i+len(o.Delimiter)]
				if !seen[cp] {
					seen[cp] = true
					res.CommonPrefixes = append(res.CommonPrefixes, cp)
					count++
					res.NextKeyMarker, res.NextUploadIDMarker = cp, ""
				}
				continue
			}
		}
		_ = json.Unmarshal([]byte(meta), &u.Metadata)
		u.Created = msToTime(created)
		res.Uploads = append(res.Uploads, u)
		count++
		res.NextKeyMarker, res.NextUploadIDMarker = u.Key, u.ID
	}
	return res, rows.Err()
}

// ---- identity ----

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var buckets string
	var disabled int
	var created int64
	if err := row.Scan(&u.ID, &u.Name, &u.Role, &buckets, &disabled, &created, &u.PasswordHash); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(buckets), &u.Buckets)
	u.Disabled = disabled == 1
	u.CreatedAt = msToTime(created)
	return &u, nil
}

// GetUser returns a user by id.
func (db *DB) GetUser(ctx context.Context, id string) (*User, error) {
	u, err := scanUser(db.Reader().QueryRowContext(ctx, `SELECT id, name, role, buckets, disabled, created_at, password_hash FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchUser
	}
	return u, err
}

// GetUserByName returns a user by name.
func (db *DB) GetUserByName(ctx context.Context, name string) (*User, error) {
	u, err := scanUser(db.Reader().QueryRowContext(ctx, `SELECT id, name, role, buckets, disabled, created_at, password_hash FROM users WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchUser
	}
	return u, err
}

// ListUsers returns all users.
func (db *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT id, name, role, buckets, disabled, created_at, password_hash FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// Credential is what the authenticator needs: key, secret, and owning user.
type Credential struct {
	Key  AccessKey
	User User
}

// LookupAccessKey returns the key and its user. Disabled keys and users are
// returned with their flags set; the caller decides.
func (db *DB) LookupAccessKey(ctx context.Context, accessKeyID string) (*Credential, error) {
	var c Credential
	var kdis, udis int
	var kcreated, ucreated int64
	var buckets string
	err := db.Reader().QueryRowContext(ctx, `SELECT k.access_key, k.secret, k.user_id, k.label, k.disabled, k.created_at,
		u.id, u.name, u.role, u.buckets, u.disabled, u.created_at
		FROM access_keys k JOIN users u ON u.id = k.user_id WHERE k.access_key = ?`, accessKeyID).
		Scan(&c.Key.AccessKeyID, &c.Key.Secret, &c.Key.UserID, &c.Key.Label, &kdis, &kcreated,
			&c.User.ID, &c.User.Name, &c.User.Role, &buckets, &udis, &ucreated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchKeyID
	}
	if err != nil {
		return nil, err
	}
	c.Key.Disabled, c.User.Disabled = kdis == 1, udis == 1
	c.Key.CreatedAt, c.User.CreatedAt = msToTime(kcreated), msToTime(ucreated)
	_ = json.Unmarshal([]byte(buckets), &c.User.Buckets)
	return &c, nil
}

// ListAccessKeys returns keys, optionally for one user. Secrets are included;
// callers must redact for display.
func (db *DB) ListAccessKeys(ctx context.Context, userID string) ([]AccessKey, error) {
	q := `SELECT access_key, secret, user_id, label, disabled, created_at FROM access_keys`
	var args []any
	if userID != "" {
		q += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	rows, err := db.Reader().QueryContext(ctx, q+` ORDER BY created_at, access_key`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AccessKey
	for rows.Next() {
		var k AccessKey
		var dis int
		var created int64
		if err := rows.Scan(&k.AccessKeyID, &k.Secret, &k.UserID, &k.Label, &dis, &created); err != nil {
			return nil, err
		}
		k.Disabled = dis == 1
		k.CreatedAt = msToTime(created)
		out = append(out, k)
	}
	return out, rows.Err()
}

// ---- settings, nodes, certs, audit, chunks ----

// GetSetting returns a server setting or "" when unset.
func (db *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := db.Reader().QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// AllSettings returns every server setting.
func (db *DB) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// GetCert returns a certificate-store entry.
func (db *DB) GetCert(ctx context.Context, key string) ([]byte, int64, error) {
	var v []byte
	var mod int64
	err := db.Reader().QueryRowContext(ctx, `SELECT value, modified FROM certs WHERE key=?`, key).Scan(&v, &mod)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, sql.ErrNoRows
	}
	return v, mod, err
}

// ListCerts returns keys under prefix. With recursive=false only direct
// children (by "/" separator) are returned.
func (db *DB) ListCerts(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT key FROM certs WHERE key LIKE ? ESCAPE '\' ORDER BY key`, likeEscape(prefix)+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		if !recursive {
			rest := strings.TrimPrefix(strings.TrimPrefix(k, prefix), "/")
			if i := strings.Index(rest, "/"); i >= 0 {
				k = strings.TrimSuffix(prefix, "/") + "/" + rest[:i]
			}
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

// ListAudit returns audit entries newest first.
func (db *DB) ListAudit(ctx context.Context, limit int, beforeID int64) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id, at, actor, action, target, detail FROM audit`
	var args []any
	if beforeID > 0 {
		q += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	rows, err := db.Reader().QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = msToTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// UnreferencedChunks returns chunk hashes with zero references released
// before the given unix-millisecond time.
func (db *DB) UnreferencedChunks(ctx context.Context, beforeMs int64, limit int) ([]Chunk, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT hash, size, refs, unref_at FROM chunks WHERE refs = 0 AND unref_at > 0 AND unref_at < ? LIMIT ?`, beforeMs, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Chunk
	for rows.Next() {
		var c Chunk
		var unref int64
		if err := rows.Scan(&c.Hash, &c.Size, &c.Refs, &unref); err != nil {
			return nil, err
		}
		c.UnrefAt = msToTime(unref)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChunkKnown reports whether a chunk row exists (referenced or not).
func (db *DB) ChunkKnown(ctx context.Context, hash string) (bool, error) {
	var n int
	err := db.Reader().QueryRowContext(ctx, `SELECT 1 FROM chunks WHERE hash=?`, hash).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Stats are summary counters for status pages.
type Stats struct {
	Buckets int64
	Objects int64
	Bytes   int64
	Uploads int64
	Chunks  int64
}

// Stats returns summary counters.
func (db *DB) Stats(ctx context.Context) (*Stats, error) {
	var s Stats
	r := db.Reader()
	if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets`).Scan(&s.Buckets); err != nil {
		return nil, err
	}
	if err := r.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size),0) FROM versions WHERE delete_marker=0`).Scan(&s.Objects, &s.Bytes); err != nil {
		return nil, err
	}
	if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM uploads WHERE completed = 0`).Scan(&s.Uploads); err != nil {
		return nil, err
	}
	if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&s.Chunks); err != nil {
		return nil, err
	}
	return &s, nil
}

// ExpiredUploads returns uploads created before the given unix ms time.
func (db *DB) ExpiredUploads(ctx context.Context, beforeMs int64, limit int) ([]Upload, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT upload_id, bucket, key, created_at FROM uploads WHERE completed = 0 AND created_at < ? LIMIT ?`, beforeMs, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Upload
	for rows.Next() {
		var u Upload
		var created int64
		if err := rows.Scan(&u.ID, &u.Bucket, &u.Key, &created); err != nil {
			return nil, err
		}
		u.Created = msToTime(created)
		out = append(out, u)
	}
	return out, rows.Err()
}

// BucketStats returns object count and bytes for one bucket (current,
// non-deleted versions).
func (db *DB) BucketStats(ctx context.Context, bucket string) (*Stats, error) {
	var s Stats
	err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size),0) FROM versions WHERE bucket=? AND is_latest=1 AND delete_marker=0`, bucket).Scan(&s.Objects, &s.Bytes)
	return &s, err
}

// BucketVersionStats counts every version row of a bucket, current or not,
// and how many of them are delete markers. A bucket with current objects
// zero but versions non-zero is "empty" to a browser yet not deletable.
func (db *DB) BucketVersionStats(ctx context.Context, bucket string) (versions, markers int64, err error) {
	err = db.Reader().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(delete_marker),0) FROM versions WHERE bucket=?`, bucket).Scan(&versions, &markers)
	return
}

// ListChunks returns referenced chunk rows with hash > after, in hash order.
func (db *DB) ListChunks(ctx context.Context, after string, limit int) ([]Chunk, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT hash, size, refs FROM chunks WHERE refs > 0 AND hash > ? ORDER BY hash LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.Hash, &c.Size, &c.Refs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CompletedUploadResult returns the stored result of an already completed
// multipart upload, so a retried CompleteMultipartUpload is idempotent.
func (db *DB) CompletedUploadResult(ctx context.Context, id string) (*CompleteUploadResult, string, string, bool) {
	var result, bucket, key string
	err := db.Reader().QueryRowContext(ctx, `SELECT result, bucket, key FROM uploads WHERE upload_id=? AND completed=1`, id).Scan(&result, &bucket, &key)
	if err != nil || result == "" {
		return nil, "", "", false
	}
	var r CompleteUploadResult
	if json.Unmarshal([]byte(result), &r) != nil {
		return nil, "", "", false
	}
	return &r, bucket, key, true
}

// ReplicationLog returns entries for bucket with seq > after.
func (db *DB) ReplicationLog(ctx context.Context, bucket string, after int64, limit int) ([]ReplicationEntry, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT seq, bucket, key, version_id, op, at, origin FROM replication_log WHERE bucket = ? AND seq > ? ORDER BY seq LIMIT ?`, bucket, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReplicationEntry
	for rows.Next() {
		var e ReplicationEntry
		var at int64
		if err := rows.Scan(&e.Seq, &e.Bucket, &e.Key, &e.VersionID, &e.Op, &at, &e.Origin); err != nil {
			return nil, err
		}
		e.At = msToTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReplicationHead returns the highest log seq for a bucket (0 when empty).
func (db *DB) ReplicationHead(ctx context.Context, bucket string) (int64, error) {
	var seq int64
	err := db.Reader().QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM replication_log WHERE bucket = ?`, bucket).Scan(&seq)
	return seq, err
}

// ReplicationLogBuckets lists the distinct buckets that still have log entries.
func (db *DB) ReplicationLogBuckets(ctx context.Context) ([]string, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT DISTINCT bucket FROM replication_log`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ReplicationLogStats returns row count and the lowest and highest seq.
func (db *DB) ReplicationLogStats(ctx context.Context) (count, minSeq, maxSeq int64, err error) {
	err = db.Reader().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0) FROM replication_log`).Scan(&count, &minSeq, &maxSeq)
	return
}

// TransferRow is a persisted transfer job record.
type TransferRow struct {
	ID      string
	Created time.Time
	Data    string // JSON managed by the transfer package
}

// ListTransfers returns job records, newest first.
func (db *DB) ListTransfers(ctx context.Context) ([]TransferRow, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT id, created, data FROM transfers ORDER BY created DESC, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TransferRow
	for rows.Next() {
		var r TransferRow
		var created int64
		if err := rows.Scan(&r.ID, &created, &r.Data); err != nil {
			return nil, err
		}
		r.Created = msToTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetTransfer returns one job record or ErrNoSuchTransfer.
func (db *DB) GetTransfer(ctx context.Context, id string) (*TransferRow, error) {
	var r TransferRow
	var created int64
	err := db.Reader().QueryRowContext(ctx, `SELECT id, created, data FROM transfers WHERE id = ?`, id).Scan(&r.ID, &created, &r.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchTransfer
	}
	if err != nil {
		return nil, err
	}
	r.Created = msToTime(created)
	return &r, nil
}

// UsageFilter narrows a usage query; empty fields match everything.
type UsageFilter struct {
	From, To  time.Time // half-open [From, To)
	Bucket    string
	User      string
	AccessKey string
}

// usageDims maps the public group-by names to columns.
var usageDims = map[string]string{"hour": "hour", "bucket": "bucket", "user": "user", "key": "access_key", "class": "class"}

// Usage sums counters grouped by the given dimensions (hour, bucket, user,
// key, class); with none it returns one total row. Rows come back ordered
// by requests descending (by hour ascending when grouping by hour).
func (db *DB) Usage(ctx context.Context, f UsageFilter, by []string) ([]UsageRow, error) {
	var cols []string
	for _, d := range by {
		c, ok := usageDims[d]
		if !ok {
			return nil, fmt.Errorf("unknown usage dimension %q (use hour, bucket, user, key or class)", d)
		}
		cols = append(cols, c)
	}
	sel := []string{"COALESCE(SUM(requests),0)", "COALESCE(SUM(errors),0)", "COALESCE(SUM(bytes_in),0)", "COALESCE(SUM(bytes_out),0)"}
	for _, c := range []string{"hour", "bucket", "user", "access_key", "class"} {
		if contains(cols, c) {
			sel = append(sel, c)
		} else if c == "hour" {
			sel = append(sel, "0")
		} else {
			sel = append(sel, "''")
		}
	}
	q := `SELECT ` + strings.Join(sel, ", ") + ` FROM usage WHERE 1=1`
	var args []any
	if !f.From.IsZero() {
		q += ` AND hour >= ?`
		args = append(args, f.From.Unix())
	}
	if !f.To.IsZero() {
		q += ` AND hour < ?`
		args = append(args, f.To.Unix())
	}
	if f.Bucket != "" {
		q += ` AND bucket = ?`
		args = append(args, f.Bucket)
	}
	if f.User != "" {
		q += ` AND user = ?`
		args = append(args, f.User)
	}
	if f.AccessKey != "" {
		q += ` AND access_key = ?`
		args = append(args, f.AccessKey)
	}
	if len(cols) > 0 {
		q += ` GROUP BY ` + strings.Join(cols, ", ")
		if contains(cols, "hour") {
			q += ` ORDER BY hour ASC, SUM(requests) DESC`
		} else {
			q += ` ORDER BY SUM(requests) DESC`
		}
	}
	rows, err := db.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Requests, &r.Errors, &r.BytesIn, &r.BytesOut, &r.Hour, &r.Bucket, &r.User, &r.AccessKey, &r.Class); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// RequestErrors returns stored failed requests (JSON) at or after since,
// newest first, at most limit.
func (db *DB) RequestErrors(ctx context.Context, since time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.Reader().QueryContext(ctx, `SELECT data FROM request_errors WHERE at >= ? ORDER BY id DESC LIMIT ?`, since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
