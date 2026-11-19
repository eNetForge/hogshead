package meta

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func decodeInto(s string, v any) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), v)
}

func (a *applier) createUser(c *CreateUser) error {
	switch c.Role {
	case RoleAdmin, RoleReadWrite, RoleReadOnly:
	default:
		return E(CodeInvalidArgument, "role must be admin, read-write or read-only")
	}
	if c.ID == "" || c.Name == "" {
		return E(CodeInvalidArgument, "user id and name are required")
	}
	var n int
	err := a.tx.QueryRowContext(a.ctx, `SELECT 1 FROM users WHERE id = ? OR name = ?`, c.ID, c.Name).Scan(&n)
	if err == nil {
		return E(CodeUserExists, "user %q already exists", c.Name)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	buckets := c.Buckets
	if buckets == nil {
		buckets = []string{}
	}
	if _, err = a.exec(`INSERT INTO users(id, name, role, buckets, created_at) VALUES(?,?,?,?,?)`,
		c.ID, c.Name, c.Role, encodeJSON(buckets), c.Now); err != nil {
		return err
	}
	return a.logServer(ReplicationUser, c.Name, "")
}

// userName resolves a user id for log entries.
func (a *applier) userName(id string) (string, error) {
	var name string
	err := a.tx.QueryRowContext(a.ctx, `SELECT name FROM users WHERE id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSuchUser
	}
	return name, err
}

func (a *applier) setUserPassword(c *SetUserPassword) error {
	name, err := a.userName(c.ID)
	if err != nil {
		return err
	}
	if _, err := a.exec(`UPDATE users SET password_hash = ? WHERE id = ?`, c.Hash, c.ID); err != nil {
		return err
	}
	return a.logServer(ReplicationUser, name, "")
}

func (a *applier) updateUser(c *UpdateUser) (err error) {
	var name string
	name, err = a.userName(c.ID)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil {
			err = a.logServer(ReplicationUser, name, "")
		}
	}()
	if c.Role != nil {
		switch *c.Role {
		case RoleAdmin, RoleReadWrite, RoleReadOnly:
		default:
			return E(CodeInvalidArgument, "role must be admin, read-write or read-only")
		}
		if _, err := a.exec(`UPDATE users SET role = ? WHERE id = ?`, *c.Role, c.ID); err != nil {
			return err
		}
	}
	if c.Buckets != nil {
		b := *c.Buckets
		if b == nil {
			b = []string{}
		}
		if _, err := a.exec(`UPDATE users SET buckets = ? WHERE id = ?`, encodeJSON(b), c.ID); err != nil {
			return err
		}
	}
	if c.Disabled != nil {
		d := 0
		if *c.Disabled {
			d = 1
		}
		if _, err := a.exec(`UPDATE users SET disabled = ? WHERE id = ?`, d, c.ID); err != nil {
			return err
		}
	}
	return nil
}

func (a *applier) deleteUser(c *DeleteUser) error {
	name, err := a.userName(c.ID)
	if err != nil {
		return err
	}
	keys, err := a.tx.QueryContext(a.ctx, `SELECT access_key FROM access_keys WHERE user_id = ?`, c.ID)
	if err != nil {
		return err
	}
	var aks []string
	for keys.Next() {
		var ak string
		if err := keys.Scan(&ak); err != nil {
			_ = keys.Close()
			return err
		}
		aks = append(aks, ak)
	}
	_ = keys.Close()
	if err := a.deleteUserRows(c); err != nil {
		return err
	}
	for _, ak := range aks {
		if err := a.logServer(ReplicationKey, ak, name); err != nil {
			return err
		}
	}
	return a.logServer(ReplicationUser, name, "")
}

func (a *applier) deleteUserRows(c *DeleteUser) error {
	res, err := a.exec(`DELETE FROM users WHERE id = ?`, c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchUser
	}
	return nil
}

func (a *applier) createAccessKey(c *CreateAccessKey) error {
	if c.AccessKeyID == "" || c.Secret == "" {
		return E(CodeInvalidArgument, "access key id and secret are required")
	}
	var n int
	if err := a.tx.QueryRowContext(a.ctx, `SELECT 1 FROM users WHERE id = ?`, c.UserID).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSuchUser
		}
		return err
	}
	if _, err := a.exec(`INSERT INTO access_keys(access_key, secret, user_id, label, created_at) VALUES(?,?,?,?,?)`,
		c.AccessKeyID, c.Secret, c.UserID, c.Label, c.Now); err != nil {
		return err
	}
	name, err := a.userName(c.UserID)
	if err != nil {
		return err
	}
	return a.logServer(ReplicationKey, c.AccessKeyID, name)
}

// keyOwner resolves the user name owning an access key.
func (a *applier) keyOwner(ak string) (string, error) {
	var name string
	err := a.tx.QueryRowContext(a.ctx, `SELECT u.name FROM access_keys k JOIN users u ON u.id = k.user_id WHERE k.access_key = ?`, ak).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSuchKeyID
	}
	return name, err
}

func (a *applier) updateAccessKey(c *UpdateAccessKey) error {
	d := 0
	if c.Disabled {
		d = 1
	}
	owner, err := a.keyOwner(c.AccessKeyID)
	if err != nil {
		return err
	}
	if _, err := a.exec(`UPDATE access_keys SET disabled = ? WHERE access_key = ?`, d, c.AccessKeyID); err != nil {
		return err
	}
	return a.logServer(ReplicationKey, c.AccessKeyID, owner)
}

func (a *applier) deleteAccessKey(c *DeleteAccessKey) error {
	owner, err := a.keyOwner(c.AccessKeyID)
	if err != nil {
		return err
	}
	if _, err := a.exec(`DELETE FROM access_keys WHERE access_key = ?`, c.AccessKeyID); err != nil {
		return err
	}
	return a.logServer(ReplicationKey, c.AccessKeyID, owner)
}

func (a *applier) setSetting(c *SetSetting) error {
	if c.Value == "" {
		if _, err := a.exec(`DELETE FROM settings WHERE key = ?`, c.Key); err != nil {
			return err
		}
	} else if _, err := a.exec(`INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, c.Key, c.Value); err != nil {
		return err
	}
	return a.logSetting(c.Key)
}

// logSetting records a setting change: bucket-scoped settings as bucket
// configuration, replicable server settings as server entries, and nothing
// for keys that are local by nature (replication state, UI secrets, ids).
func (a *applier) logSetting(key string) error {
	for _, kind := range []string{"lock", "tagging", "notification"} {
		if bucket, ok := strings.CutPrefix(key, "bucket."+kind+":"); ok {
			return a.logBucketConfig(bucket, kind)
		}
	}
	if ReplicableSetting(key) {
		return a.logServer(ReplicationSetting, key, "")
	}
	return nil
}

func (a *applier) putCert(c *PutCert) error {
	_, err := a.exec(`INSERT INTO certs(key, value, modified) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, modified=excluded.modified`, c.Key, c.Value, c.Now)
	return err
}

func (a *applier) deleteCert(c *DeleteCert) error {
	if c.Prefix {
		_, err := a.exec(`DELETE FROM certs WHERE key = ? OR key LIKE ? ESCAPE '\'`, c.Key, likeEscape(c.Key)+"/%")
		return err
	}
	_, err := a.exec(`DELETE FROM certs WHERE key = ?`, c.Key)
	return err
}

func (a *applier) addUsage(c *AddUsage) error {
	for _, r := range c.Rows {
		if _, err := a.exec(`INSERT INTO usage(hour, bucket, user, access_key, class, requests, errors, bytes_in, bytes_out)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(hour, bucket, user, access_key, class) DO UPDATE SET
			requests = requests + excluded.requests, errors = errors + excluded.errors,
			bytes_in = bytes_in + excluded.bytes_in, bytes_out = bytes_out + excluded.bytes_out`,
			r.Hour, r.Bucket, r.User, r.AccessKey, r.Class, r.Requests, r.Errors, r.BytesIn, r.BytesOut); err != nil {
			return err
		}
	}
	return nil
}

func (a *applier) putTransfer(c *PutTransfer) error {
	_, err := a.exec(`INSERT INTO transfers(id, created, data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`, c.ID, c.Created, c.Data)
	return err
}

func (a *applier) deleteTransfer(c *DeleteTransfer) error {
	res, err := a.exec(`DELETE FROM transfers WHERE id = ?`, c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return E(CodeNoSuchTransfer, "no such transfer")
	}
	return nil
}

func (a *applier) audit(c *Audit) error {
	_, err := a.exec(`INSERT INTO audit(at, actor, action, target, detail) VALUES(?,?,?,?,?)`,
		c.Now, c.Actor, c.Action, c.Target, c.Detail)
	return err
}
