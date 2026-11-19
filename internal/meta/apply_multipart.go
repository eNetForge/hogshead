package meta

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const minPartSize = 5 * 1024 * 1024

func (a *applier) createUpload(c *CreateUpload) error {
	ok, err := a.bucketExists(c.Bucket)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoSuchBucket
	}
	meta := c.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	_, err = a.exec(`INSERT INTO uploads(upload_id, bucket, key, owner, content_type, metadata, checksum_algo, checksum_type, storage_class, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		c.UploadID, c.Bucket, c.Key, c.Owner, c.ContentType, encodeJSON(meta), c.ChecksumAlgo, c.ChecksumType, c.StorageClass, c.Now)
	return err
}

func (a *applier) uploadInfo(id string) (bucket, key string, err error) {
	err = a.tx.QueryRowContext(a.ctx, `SELECT bucket, key FROM uploads WHERE upload_id = ? AND completed = 0`, id).Scan(&bucket, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNoSuchUpload
	}
	return bucket, key, err
}

func (a *applier) putPart(c *PutPart) error {
	if _, _, err := a.uploadInfo(c.UploadID); err != nil {
		return err
	}
	if c.Number < 1 || c.Number > 10000 {
		return E(CodeInvalidArgument, "part number must be between 1 and 10000")
	}
	// Replace an existing part: release its chunks first.
	var old string
	err := a.tx.QueryRowContext(a.ctx, `SELECT chunks FROM parts WHERE upload_id=? AND part_number=?`, c.UploadID, c.Number).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if err := a.releaseRefs(decodeChunks(old), c.Now); err != nil {
			return err
		}
	}
	chunks := c.Chunks
	if chunks == nil {
		chunks = []ChunkRef{}
	}
	if _, err := a.exec(`INSERT OR REPLACE INTO parts(upload_id, part_number, size, etag, chunks, checksums, created_at) VALUES(?,?,?,?,?,?,?)`,
		c.UploadID, c.Number, c.Size, c.ETag, encodeJSON(chunks), encodeJSON(c.Checksums), c.Now); err != nil {
		return err
	}
	return a.addRefs(chunks)
}

func (a *applier) completeUpload(c *CompleteUpload) (any, error) {
	bucket, key, err := a.uploadInfo(c.UploadID)
	if err != nil {
		// A retried completion returns the first result.
		var result string
		if qerr := a.tx.QueryRowContext(a.ctx, `SELECT result FROM uploads WHERE upload_id = ? AND completed = 1`, c.UploadID).Scan(&result); qerr == nil && result != "" {
			var prev CompleteUploadResult
			if json.Unmarshal([]byte(result), &prev) == nil {
				return &prev, nil
			}
		}
		return nil, err
	}
	var owner, ct, metaJSON, sc string
	if err := a.tx.QueryRowContext(a.ctx, `SELECT owner, content_type, metadata, storage_class FROM uploads WHERE upload_id=?`, c.UploadID).
		Scan(&owner, &ct, &metaJSON, &sc); err != nil {
		return nil, err
	}
	if len(c.Parts) == 0 {
		return nil, E(CodeInvalidArgument, "at least one part is required")
	}
	rows, err := a.tx.QueryContext(a.ctx, `SELECT part_number, size, etag, chunks FROM parts WHERE upload_id=? ORDER BY part_number`, c.UploadID)
	if err != nil {
		return nil, err
	}
	type prow struct {
		size   int64
		etag   string
		chunks []ChunkRef
	}
	stored := map[int]prow{}
	for rows.Next() {
		var n int
		var p prow
		var ch string
		if err := rows.Scan(&n, &p.size, &p.etag, &ch); err != nil {
			_ = rows.Close()
			return nil, err
		}
		p.chunks = decodeChunks(ch)
		stored[n] = p
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var all []ChunkRef
	var size int64
	last := 0
	for i, cp := range c.Parts {
		if cp.Number <= last {
			return nil, E(CodeInvalidPartOrder, "parts must be in ascending order")
		}
		last = cp.Number
		p, ok := stored[cp.Number]
		if !ok || trimETag(strings.ToLower(p.etag)) != trimETag(strings.ToLower(cp.ETag)) {
			return nil, E(CodeInvalidPart, "part %d not found or etag mismatch", cp.Number)
		}
		if i < len(c.Parts)-1 && p.size < minPartSize {
			return nil, E(CodeEntityTooSmall, "part %d is smaller than %d bytes", cp.Number, minPartSize)
		}
		all = append(all, p.chunks...)
		size += p.size
	}

	versioning, _, err := a.bucketVersioning(bucket)
	if err != nil {
		return nil, err
	}
	meta := map[string]string{}
	_ = decodeInto(metaJSON, &meta)
	for k, v := range c.ExtraMetadata {
		meta[k] = v
	}
	put := &PutObjectVersion{
		Bucket: bucket, Key: key, VersionID: c.VersionID, Size: size, ETag: c.ETag,
		ContentType: ct, Metadata: meta, Chunks: all, Checksums: c.Checksums,
		StorageClass: sc, Owner: owner, Now: c.Now, IfNoneMatchAny: c.IfNoneMatchAny,
	}
	latest, err := a.latestVersion(bucket, key)
	if err != nil {
		return nil, err
	}
	if put.IfNoneMatchAny && latest != nil && !latest.deleteMarker {
		return nil, E(CodePreconditionFailed, "object already exists")
	}
	vid := c.VersionID
	if versioning != VersioningEnabled {
		vid = NullVersionID
		if _, err := a.removeVersionRow(bucket, key, NullVersionID, c.Now); err != nil {
			return nil, err
		}
	}
	if err := a.insertVersion(put, vid, false); err != nil {
		return nil, err
	}
	// Drop the parts (their chunk refs are released, version refs stay) but
	// keep the upload row marked completed so a retry is idempotent.
	res := &CompleteUploadResult{VersionID: vid, ETag: c.ETag, Size: size}
	if err := a.releasePartRows(c.UploadID, c.Now); err != nil {
		return nil, err
	}
	if _, err := a.exec(`UPDATE uploads SET completed = 1, result = ?, created_at = ? WHERE upload_id = ?`, encodeJSON(res), c.Now, c.UploadID); err != nil {
		return nil, err
	}
	return res, nil
}

// releasePartRows releases part chunk refs and deletes the part rows.
func (a *applier) releasePartRows(id string, now int64) error {
	rows, err := a.tx.QueryContext(a.ctx, `SELECT chunks FROM parts WHERE upload_id=?`, id)
	if err != nil {
		return err
	}
	var all []ChunkRef
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, decodeChunks(ch)...)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := a.releaseRefs(all, now); err != nil {
		return err
	}
	_, err = a.exec(`DELETE FROM parts WHERE upload_id=?`, id)
	return err
}

func (a *applier) abortUploadRows(id string, now int64) error {
	if err := a.releasePartRows(id, now); err != nil {
		return err
	}
	_, err := a.exec(`DELETE FROM uploads WHERE upload_id=?`, id)
	return err
}

func (a *applier) abortUpload(c *AbortUpload) error {
	if _, _, err := a.uploadInfo(c.UploadID); err != nil {
		return err
	}
	return a.abortUploadRows(c.UploadID, c.Now)
}
