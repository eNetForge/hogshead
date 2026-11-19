package meta

import (
	"database/sql"
	"errors"
)

func (a *applier) bucketExists(name string) (bool, error) {
	var n int
	err := a.tx.QueryRowContext(a.ctx, `SELECT 1 FROM buckets WHERE name = ?`, name).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (a *applier) bucketVersioning(name string) (string, bool, error) {
	var v string
	var lock int
	err := a.tx.QueryRowContext(a.ctx, `SELECT versioning, object_lock FROM buckets WHERE name = ?`, name).Scan(&v, &lock)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNoSuchBucket
	}
	return v, lock == 1, err
}

func (a *applier) createBucket(c *CreateBucket) error {
	var owner string
	err := a.tx.QueryRowContext(a.ctx, `SELECT owner FROM buckets WHERE name = ?`, c.Name).Scan(&owner)
	if err == nil {
		if owner == c.Owner {
			return E(CodeBucketAlreadyOwned, "bucket %q already exists", c.Name)
		}
		return E(CodeBucketAlreadyExists, "bucket %q already exists", c.Name)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	versioning := ""
	lock := 0
	if c.ObjectLock {
		versioning = VersioningEnabled
		lock = 1
	}
	_, err = a.exec(`INSERT INTO buckets(name, owner, created_at, versioning, object_lock) VALUES(?,?,?,?,?)`,
		c.Name, c.Owner, c.Now, versioning, lock)
	if err != nil {
		return err
	}
	return a.logBucketConfig(c.Name, "create")
}

func (a *applier) deleteBucket(c *DeleteBucket) error {
	ok, err := a.bucketExists(c.Name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoSuchBucket
	}
	var n int
	if err := a.tx.QueryRowContext(a.ctx, `SELECT COUNT(*) FROM versions WHERE bucket = ?`, c.Name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return E(CodeBucketNotEmpty, "bucket %q is not empty", c.Name)
	}
	// In-progress multipart uploads are discarded, as on AWS general
	// purpose buckets.
	rows, err := a.tx.QueryContext(a.ctx, `SELECT upload_id FROM uploads WHERE bucket = ? AND completed = 0`, c.Name)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	for _, id := range ids {
		if err := a.abortUploadRows(id, c.Now); err != nil {
			return err
		}
	}
	if _, err := a.exec(`DELETE FROM uploads WHERE bucket = ?`, c.Name); err != nil {
		return err
	}
	_, err = a.exec(`DELETE FROM buckets WHERE name = ?`, c.Name)
	if err != nil {
		return err
	}
	return a.logBucketConfig(c.Name, "delete")
}

func (a *applier) setBucketVersioning(c *SetBucketVersioning) error {
	switch c.State {
	case VersioningEnabled, VersioningSuspended:
	default:
		return E(CodeInvalidArgument, "versioning state must be Enabled or Suspended")
	}
	_, lock, err := a.bucketVersioning(c.Name)
	if err != nil {
		return err
	}
	if lock && c.State == VersioningSuspended {
		return E(CodeInvalidBucketState, "versioning cannot be suspended on an Object Lock bucket")
	}
	if _, err = a.exec(`UPDATE buckets SET versioning = ? WHERE name = ?`, c.State, c.Name); err != nil {
		return err
	}
	return a.logBucketConfig(c.Name, "versioning")
}

func (a *applier) setBucketField(name, field, value string) error {
	res, err := a.exec(`UPDATE buckets SET `+field+` = ? WHERE name = ?`, value, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNoSuchBucket
	}
	return a.logBucketConfig(name, field)
}
