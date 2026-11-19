package meta

import (
	"database/sql"
	"errors"
	"time"
)

type versionRow struct {
	versionID    string
	deleteMarker bool
	etag         string
	chunks       []ChunkRef
	created      int64 // unix ms
}

func (a *applier) latestVersion(bucket, key string) (*versionRow, error) {
	var r versionRow
	var dm int
	var chunks string
	err := a.tx.QueryRowContext(a.ctx,
		`SELECT version_id, delete_marker, etag, chunks, created_at FROM versions WHERE bucket=? AND key=? AND is_latest=1`,
		bucket, key).Scan(&r.versionID, &dm, &r.etag, &chunks, &r.created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.deleteMarker = dm == 1
	r.chunks = decodeChunks(chunks)
	return &r, nil
}

// removeVersionRow deletes a version row and releases its chunks.
func (a *applier) removeVersionRow(bucket, key, vid string, now int64) (bool, error) {
	var chunks string
	err := a.tx.QueryRowContext(a.ctx, `SELECT chunks FROM versions WHERE bucket=? AND key=? AND version_id=?`,
		bucket, key, vid).Scan(&chunks)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := a.exec(`DELETE FROM versions WHERE bucket=? AND key=? AND version_id=?`, bucket, key, vid); err != nil {
		return false, err
	}
	if err := a.logReplication(bucket, key, vid, ReplicationDelete, now); err != nil {
		return false, err
	}
	return true, a.releaseRefs(decodeChunks(chunks), now)
}

// promoteLatest marks the most recently created remaining version of key as
// latest (ties broken by arrival order).
func (a *applier) promoteLatest(bucket, key string) error {
	_, err := a.exec(`UPDATE versions SET is_latest = 1 WHERE bucket=? AND key=? AND seq =
		(SELECT seq FROM versions WHERE bucket=? AND key=? ORDER BY created_at DESC, seq DESC LIMIT 1)`, bucket, key, bucket, key)
	return err
}

// versionExists reports whether a version row is already present.
func (a *applier) versionExists(bucket, key, vid string) (bool, error) {
	var n int
	err := a.tx.QueryRowContext(a.ctx, `SELECT 1 FROM versions WHERE bucket=? AND key=? AND version_id=?`, bucket, key, vid).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (a *applier) insertVersion(v *PutObjectVersion, vid string, deleteMarker bool) error {
	// Idempotent: a version this server already holds (a replicated change
	// arriving twice) is not inserted or logged again.
	if vid != NullVersionID {
		if exists, err := a.versionExists(v.Bucket, v.Key, vid); err != nil || exists {
			return err
		}
	}
	seq, err := a.nextSeq()
	if err != nil {
		return err
	}
	// "Latest" follows the write's original time, not arrival order, so a
	// change that was delayed in replication never overtakes a newer one.
	// Equal timestamps fall back to arrival order.
	isLatest := 1
	var latestCreated int64
	err = a.tx.QueryRowContext(a.ctx, `SELECT created_at FROM versions WHERE bucket=? AND key=? AND is_latest=1`, v.Bucket, v.Key).Scan(&latestCreated)
	switch {
	case err == nil && latestCreated > v.Now:
		isLatest = 0
	case err == nil:
		if _, err := a.exec(`UPDATE versions SET is_latest = 0 WHERE bucket=? AND key=? AND is_latest=1`, v.Bucket, v.Key); err != nil {
			return err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	dm := 0
	if deleteMarker {
		dm = 1
	}
	meta := v.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	chunks := v.Chunks
	if chunks == nil {
		chunks = []ChunkRef{}
	}
	_, err = a.exec(`INSERT INTO versions(bucket, key, version_id, seq, is_latest, delete_marker, size, etag,
		content_type, metadata, chunks, checksums, storage_class, owner, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		v.Bucket, v.Key, vid, seq, isLatest, dm, v.Size, v.ETag, v.ContentType, encodeJSON(meta),
		encodeJSON(chunks), encodeJSON(v.Checksums), v.StorageClass, v.Owner, v.Now)
	if err != nil {
		return err
	}
	op := ReplicationPut
	if deleteMarker {
		op = ReplicationMarker
	}
	if err := a.logReplication(v.Bucket, v.Key, vid, op, v.Now); err != nil {
		return err
	}
	if !deleteMarker {
		return a.addRefs(chunks)
	}
	return nil
}

// logReplication records a change for the replication worker.
func (a *applier) logReplication(bucket, key, vid, op string, now int64) error {
	_, err := a.exec(`INSERT INTO replication_log(bucket, key, version_id, op, at, origin) VALUES(?,?,?,?,?,?)`, bucket, key, vid, op, now, a.origin)
	return err
}

// logBucketConfig records a bucket configuration change of the given kind.
func (a *applier) logBucketConfig(bucket, kind string) error {
	return a.logReplication(bucket, kind, "", ReplicationBucket, nowMs())
}

// logServer records a server-level change (users, keys, settings).
func (a *applier) logServer(op, key, ref string) error {
	return a.logReplication("", key, ref, op, nowMs())
}

func (a *applier) putObject(c *PutObjectVersion) (any, error) {
	versioning, _, err := a.bucketVersioning(c.Bucket)
	if err != nil {
		return nil, err
	}
	latest, err := a.latestVersion(c.Bucket, c.Key)
	if err != nil {
		return nil, err
	}
	if c.IfNoneMatchAny && latest != nil && !latest.deleteMarker {
		return nil, E(CodePreconditionFailed, "object already exists")
	}
	if c.IfMatchETag != "" {
		if latest == nil || latest.deleteMarker {
			return nil, E(CodeNoSuchKey, "the object does not exist")
		}
		if trimETag(latest.etag) != trimETag(c.IfMatchETag) {
			return nil, E(CodePreconditionFailed, "etag mismatch")
		}
	}
	if c.IfNoneMatchETag != "" && latest != nil && !latest.deleteMarker && trimETag(latest.etag) == trimETag(c.IfNoneMatchETag) {
		return nil, E(CodePreconditionFailed, "etag matches")
	}
	vid := c.VersionID
	if versioning != VersioningEnabled {
		vid = NullVersionID
		// On an unversioned bucket a stale replicated overwrite must not
		// replace a newer object; it is simply dropped.
		if latest != nil && latest.created > c.Now {
			return &PutObjectResult{VersionID: NullVersionID}, nil
		}
		if _, err := a.removeVersionRow(c.Bucket, c.Key, NullVersionID, c.Now); err != nil {
			return nil, err
		}
	}
	if vid == "" {
		return nil, E(CodeInvalidArgument, "version id required")
	}
	if err := a.insertVersion(c, vid, false); err != nil {
		return nil, err
	}
	return &PutObjectResult{VersionID: vid}, nil
}

func trimETag(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func (a *applier) deleteObject(c *DeleteObject) (any, error) {
	versioning, _, err := a.bucketVersioning(c.Bucket)
	if err != nil {
		return nil, err
	}
	return a.deleteObjectIn(versioning, c)
}

func (a *applier) deleteObjectIn(versioning string, c *DeleteObject) (*DeleteObjectResult, error) {
	res := &DeleteObjectResult{}
	if c.VersionID != "" {
		// Delete a specific version (or delete marker).
		var dm int
		var wasLatest int
		err := a.tx.QueryRowContext(a.ctx, `SELECT delete_marker, is_latest FROM versions WHERE bucket=? AND key=? AND version_id=?`,
			c.Bucket, c.Key, c.VersionID).Scan(&dm, &wasLatest)
		if errors.Is(err, sql.ErrNoRows) {
			// S3 returns 204 for unknown versions on versioned buckets.
			return res, nil
		}
		if err != nil {
			return nil, err
		}
		if _, err := a.removeVersionRow(c.Bucket, c.Key, c.VersionID, c.Now); err != nil {
			return nil, err
		}
		if wasLatest == 1 {
			if err := a.promoteLatest(c.Bucket, c.Key); err != nil {
				return nil, err
			}
		}
		res.Existed = true
		res.VersionID = c.VersionID
		res.DeleteMarker = dm == 1
		return res, nil
	}

	switch versioning {
	case VersioningOff:
		existed, err := a.removeVersionRow(c.Bucket, c.Key, NullVersionID, c.Now)
		if err != nil {
			return nil, err
		}
		res.Existed = existed
		return res, nil
	case VersioningSuspended:
		// A suspended bucket's delete removes the null version and leaves a
		// null delete marker.
		if _, err := a.removeVersionRow(c.Bucket, c.Key, NullVersionID, c.Now); err != nil {
			return nil, err
		}
		if err := a.insertVersion(&PutObjectVersion{Bucket: c.Bucket, Key: c.Key, Now: c.Now}, NullVersionID, true); err != nil {
			return nil, err
		}
		res.DeleteMarker, res.VersionID = true, NullVersionID
		return res, nil
	default:
		if c.MarkerVersionID == "" {
			return nil, errors.New("delete marker version id required")
		}
		if err := a.insertVersion(&PutObjectVersion{Bucket: c.Bucket, Key: c.Key, Now: c.Now}, c.MarkerVersionID, true); err != nil {
			return nil, err
		}
		res.DeleteMarker, res.VersionID = true, c.MarkerVersionID
		return res, nil
	}
}

func (a *applier) deleteObjects(c *DeleteObjects) (any, error) {
	versioning, _, err := a.bucketVersioning(c.Bucket)
	if err != nil {
		return nil, err
	}
	out := &DeleteObjectsResult{Results: make([]DeleteObjectItemResult, 0, len(c.Items))}
	for i := range c.Items {
		item := c.Items[i]
		item.Bucket = c.Bucket
		if item.Now == 0 {
			item.Now = c.Now
		}
		if _, err := a.exec(`SAVEPOINT item`); err != nil {
			return nil, err
		}
		r, err := a.deleteObjectIn(versioning, &item)
		var ir DeleteObjectItemResult
		if err != nil {
			if _, rerr := a.exec(`ROLLBACK TO SAVEPOINT item`); rerr != nil {
				return nil, rerr
			}
			var me *Error
			if !errors.As(err, &me) {
				return nil, err
			}
			ir.Err, ir.Msg = me.Code, me.Msg
		} else {
			ir.DeleteObjectResult = *r
		}
		if _, err := a.exec(`RELEASE SAVEPOINT item`); err != nil {
			return nil, err
		}
		out.Results = append(out.Results, ir)
	}
	return out, nil
}

func (a *applier) updateObjectMeta(c *UpdateObjectMeta) error {
	var vid, metaJSON string
	var err error
	if c.VersionID == "" {
		err = a.tx.QueryRowContext(a.ctx, `SELECT version_id, metadata FROM versions WHERE bucket=? AND key=? AND is_latest=1 AND delete_marker=0`, c.Bucket, c.Key).Scan(&vid, &metaJSON)
	} else {
		vid = c.VersionID
		err = a.tx.QueryRowContext(a.ctx, `SELECT version_id, metadata FROM versions WHERE bucket=? AND key=? AND version_id=?`, c.Bucket, c.Key, vid).Scan(&vid, &metaJSON)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if c.VersionID != "" {
			return ErrNoSuchVersion
		}
		return ErrNoSuchKey
	}
	if err != nil {
		return err
	}
	m := map[string]string{}
	_ = decodeInto(metaJSON, &m)
	for _, k := range c.Remove {
		delete(m, k)
	}
	for k, v := range c.Set {
		m[k] = v
	}
	if _, err = a.exec(`UPDATE versions SET metadata=? WHERE bucket=? AND key=? AND version_id=?`, encodeJSON(m), c.Bucket, c.Key, vid); err != nil {
		return err
	}
	return a.logReplication(c.Bucket, c.Key, vid, ReplicationMeta, nowMs())
}

// nowMs is the wall clock for configuration log entries, which carry no
// caller timestamp. The value is informational only.
func nowMs() int64 { return time.Now().UnixMilli() }
