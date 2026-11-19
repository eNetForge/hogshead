package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/enetforge/hogshead/internal/api"
	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/discovery"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/policy"
)

func (h *Handler) coreRoutes() []Route {
	return []Route{
		{"GET", "/status", "Server status", false, h.status},
		{"GET", "/me", "The calling identity", false, h.me},
		{"GET", "/openapi.json", "OpenAPI description of this API", false, h.openapi},
		{"GET", "/users", "List users", true, h.listUsers},
		{"POST", "/users", "Create a user", true, h.createUser},
		{"GET", "/users/{name}", "Get a user", true, h.getUser},
		{"PATCH", "/users/{name}", "Update a user's role, bucket scope or disabled flag", true, h.updateUser},
		{"DELETE", "/users/{name}", "Delete a user and its keys", true, h.deleteUser},
		{"PUT", "/users/{name}/password", "Set or clear a user's web UI password (admins, or the user themselves)", false, h.setPassword},
		{"PUT", "/users/{name}", "Create or replace a user wholesale, including the password hash (config sync)", true, h.importUser},
		{"PUT", "/users/{name}/keys/{ak}", "Create or replace an access key with a known secret (config sync)", true, h.importKey},
		{"GET", "/peers", "hogshead servers heard on the local network", true, h.peers},
		{"GET", "/users/{name}/keys", "List a user's access keys", true, h.listKeys},
		{"POST", "/users/{name}/keys", "Create an access key; the secret is returned once", true, h.createKey},
		{"GET", "/keys", "List all access keys", true, h.listAllKeys},
		{"PATCH", "/keys/{id}", "Enable or disable an access key", true, h.updateKey},
		{"DELETE", "/keys/{id}", "Revoke an access key", true, h.deleteKey},
		{"GET", "/buckets", "List buckets", true, h.listBuckets},
		{"POST", "/buckets", "Create a bucket", true, h.createBucket},
		{"GET", "/buckets/{name}", "Get a bucket with its policy, CORS and lifecycle", true, h.getBucket},
		{"PATCH", "/buckets/{name}", "Update bucket settings", true, h.updateBucket},
		{"DELETE", "/buckets/{name}", "Delete an empty bucket", true, h.deleteBucket},
		{"GET", "/audit", "Audit log, newest first", true, h.listAudit},
		{"GET", "/settings", "Server settings", true, h.listSettings},
		{"PUT", "/settings/{key}", "Set a server setting; empty value removes it", true, h.putSetting},
		{"GET", "/config", "Node-local configuration", true, h.getConfig},
		{"POST", "/config/reload", "Re-read the node-local configuration file", true, h.reloadConfig},
	}
}

func (h *Handler) status(c *Call) error {
	ctx := c.Ctx()
	st := Status{
		Version: h.d.Version, StartedAt: h.d.Started, Uptime: h.d.Now().Sub(h.d.Started).Round(time.Second).String(),
		PendingRestart: []string{}, Settings: map[string]string{},
	}
	cfg := h.d.Cfg()
	st.Listen, st.TLSMode = cfg.Listen, cfg.TLS.Mode
	if h.d.PendingRestart != nil {
		if p := h.d.PendingRestart(); p != nil {
			st.PendingRestart = p
		}
	}
	host, _ := os.Hostname()
	st.NodeName = host
	if h.d.Name != nil {
		if n := h.d.Name(); n != "" {
			st.NodeName = n
		}
	}
	if h.d.ServerID != nil {
		st.ID = h.d.ServerID()
	}
	if s, err := h.d.DB.Stats(ctx); err == nil {
		st.Stats = Stats{Buckets: s.Buckets, Objects: s.Objects, Bytes: s.Bytes, Uploads: s.Uploads, Chunks: s.Chunks}
	}
	if h.d.WorkerStats != nil {
		st.Workers = h.d.WorkerStats()
	}
	if s, err := h.d.DB.AllSettings(ctx); err == nil {
		st.Settings = s
	}
	return c.JSON(http.StatusOK, st)
}

func (h *Handler) me(c *Call) error {
	out := Me{Via: c.Via}
	if c.User != nil {
		out.User = userOut(c.User)
	} else {
		out.User = User{Name: "local", Role: meta.RoleAdmin, Buckets: []string{}}
	}
	return c.JSON(http.StatusOK, out)
}

// ---- users ----

func (h *Handler) listUsers(c *Call) error {
	users, err := h.d.DB.ListUsers(c.Ctx())
	if err != nil {
		return err
	}
	keys, _ := h.d.DB.ListAccessKeys(c.Ctx(), "")
	count := map[string]int{}
	for _, k := range keys {
		count[k.UserID]++
	}
	out := make([]User, 0, len(users))
	for i := range users {
		u := userOut(&users[i])
		u.Keys = count[users[i].ID]
		out = append(out, u)
	}
	return c.JSON(http.StatusOK, out)
}

func validUserName(n string) bool {
	if len(n) < 1 || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' && r != '@' {
			return false
		}
	}
	return true
}

func (h *Handler) createUser(c *Call) error {
	var req CreateUserRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	if err := h.reservedUser(c, req.Name); err != nil {
		return err
	}
	if !validUserName(req.Name) {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "user name must be 1-64 characters of letters, digits, - _ . @")
	}
	if req.Role == "" {
		req.Role = meta.RoleReadWrite
	}
	id := ids.UserID()
	if _, err := h.propose(c, &meta.CreateUser{ID: id, Name: req.Name, Role: req.Role, Buckets: req.Buckets, Now: h.d.Now().UnixMilli()}); err != nil {
		return err
	}
	h.audit(c, "user.create", req.Name, req.Role)
	u, err := h.d.DB.GetUser(c.Ctx(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, userOut(u))
}

func (h *Handler) userByName(c *Call) (*meta.User, error) {
	u, err := h.d.DB.GetUserByName(c.Ctx(), c.Params["name"])
	if err != nil {
		return nil, Errorf(http.StatusNotFound, "NoSuchUser", "user %q not found", c.Params["name"])
	}
	return u, nil
}

func (h *Handler) getUser(c *Call) error {
	u, err := h.userByName(c)
	if err != nil {
		return err
	}
	out := userOut(u)
	keys, _ := h.d.DB.ListAccessKeys(c.Ctx(), u.ID)
	out.Keys = len(keys)
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) updateUser(c *Call) error {
	u, err := h.userByName(c)
	if err != nil {
		return err
	}
	if err := h.protectUser(c, u.Name); err != nil {
		return err
	}
	var req UpdateUserRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	if c.User != nil && c.User.ID == u.ID && req.Disabled != nil && *req.Disabled {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "you cannot disable yourself")
	}
	if _, err := h.propose(c, &meta.UpdateUser{ID: u.ID, Role: req.Role, Buckets: req.Buckets, Disabled: req.Disabled}); err != nil {
		return err
	}
	detail, _ := json.Marshal(req)
	h.audit(c, "user.update", u.Name, string(detail))
	u, _ = h.d.DB.GetUser(c.Ctx(), u.ID)
	return c.JSON(http.StatusOK, userOut(u))
}

// settingWritable guards the generic settings route. Server identity and web
// UI secrets are never written through it; cellar state only by a member of
// the same cellar (that is, by its config sync), since cellar settings are
// managed on the Cellar page and arriving at a non-member would make it
// believe it had joined.
func (h *Handler) settingWritable(c *Call, key, value string) error {
	switch {
	case strings.HasPrefix(key, "server."), strings.HasPrefix(key, "ui."):
		return Errorf(http.StatusForbidden, "Forbidden", "%s is managed by the server and cannot be set", key)
	case strings.HasPrefix(key, "cellar."):
		id, _ := h.d.DB.GetSetting(c.Ctx(), "cellar.id")
		if id == "" {
			return Errorf(http.StatusConflict, "NotInCellar", "this server is not in a cellar; cellar settings are managed through the Cellar page or 'hogshead cellar'")
		}
		if key == "cellar.id" && value != id {
			return Errorf(http.StatusConflict, "Conflict", "this server belongs to another cellar")
		}
	}
	return nil
}

// importUser creates or replaces a user from a full description; used by
// configuration sync between servers and by backups.
func (h *Handler) importUser(c *Call) error {
	name := c.Params["name"]
	var req UserImportRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	if err := h.reservedUser(c, name); err != nil {
		return err
	}
	if req.Buckets == nil {
		req.Buckets = []string{}
	}
	u, err := h.d.DB.GetUserByName(c.Ctx(), name)
	switch {
	case errors.Is(err, meta.ErrNoSuchUser):
		id := ids.UserID()
		if _, err := h.propose(c, &meta.CreateUser{ID: id, Name: name, Role: req.Role, Buckets: req.Buckets, Now: h.d.Now().UnixMilli()}); err != nil {
			return err
		}
		u, err = h.d.DB.GetUser(c.Ctx(), id)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if c.User != nil && c.User.ID == u.ID && req.Disabled {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "you cannot disable yourself")
	}
	role, buckets, disabled := req.Role, req.Buckets, req.Disabled
	if _, err := h.propose(c, &meta.UpdateUser{ID: u.ID, Role: &role, Buckets: &buckets, Disabled: &disabled}); err != nil {
		return err
	}
	if u.PasswordHash != req.PasswordHash {
		if _, err := h.propose(c, &meta.SetUserPassword{ID: u.ID, Hash: req.PasswordHash}); err != nil {
			return err
		}
	}
	h.audit(c, "user.import", name, "")
	u, _ = h.d.DB.GetUser(c.Ctx(), u.ID)
	return c.JSON(http.StatusOK, userOut(u))
}

// importKey creates or replaces an access key whose id and secret are known.
func (h *Handler) importKey(c *Call) error {
	u, err := h.userByName(c)
	if err != nil {
		return err
	}
	ak := c.Params["ak"]
	var req KeyImportRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	if ak == "" || req.Secret == "" {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "access key id and secret are required")
	}
	if cur, err := h.d.DB.LookupAccessKey(c.Ctx(), ak); err == nil {
		if cur.User.ID == u.ID && cur.Key.Secret == req.Secret && cur.Key.Label == req.Label {
			if cur.Key.Disabled != req.Disabled {
				if _, err := h.propose(c, &meta.UpdateAccessKey{AccessKeyID: ak, Disabled: req.Disabled}); err != nil {
					return err
				}
			}
			h.audit(c, "key.import", u.Name, ak)
			return c.JSON(http.StatusOK, keyOut(meta.AccessKey{AccessKeyID: ak, UserID: u.ID, Label: req.Label, Disabled: req.Disabled, CreatedAt: cur.Key.CreatedAt}, u.Name))
		}
		if _, err := h.propose(c, &meta.DeleteAccessKey{AccessKeyID: ak}); err != nil {
			return err
		}
	}
	now := h.d.Now()
	if _, err := h.propose(c, &meta.CreateAccessKey{AccessKeyID: ak, Secret: req.Secret, UserID: u.ID, Label: req.Label, Now: now.UnixMilli()}); err != nil {
		return err
	}
	if req.Disabled {
		if _, err := h.propose(c, &meta.UpdateAccessKey{AccessKeyID: ak, Disabled: true}); err != nil {
			return err
		}
	}
	h.audit(c, "key.import", u.Name, ak)
	return c.JSON(http.StatusOK, keyOut(meta.AccessKey{AccessKeyID: ak, UserID: u.ID, Label: req.Label, Disabled: req.Disabled, CreatedAt: now}, u.Name))
}

// peers lists servers heard on the LAN.
func (h *Handler) peers(c *Call) error {
	out := []discovery.Peer{}
	if h.d.Peers != nil {
		if p := h.d.Peers(); p != nil {
			out = p
		}
	}
	return c.JSON(http.StatusOK, out)
}

// setPassword lets an admin set any user's password and a user set their own.
func (h *Handler) setPassword(c *Call) error {
	u, err := h.userByName(c)
	if err != nil {
		return err
	}
	if err := h.protectUser(c, u.Name); err != nil {
		return err
	}
	self := c.User != nil && c.User.ID == u.ID
	if c.Via != "socket" && !self && (c.User == nil || c.User.Role != meta.RoleAdmin) {
		return Errorf(http.StatusForbidden, "Forbidden", "only administrators can set another user's password")
	}
	var req PasswordRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	hash := ""
	if req.Password != "" {
		hash, err = HashPassword(req.Password)
		if err != nil {
			return Errorf(http.StatusBadRequest, "InvalidArgument", "%v", err)
		}
	}
	if _, err := h.propose(c, &meta.SetUserPassword{ID: u.ID, Hash: hash}); err != nil {
		return err
	}
	action := "user.password.set"
	if hash == "" {
		action = "user.password.clear"
	}
	h.audit(c, action, u.Name, "")
	u, _ = h.d.DB.GetUser(c.Ctx(), u.ID)
	return c.JSON(http.StatusOK, userOut(u))
}

func (h *Handler) deleteUser(c *Call) error {
	u, err := h.userByName(c)
	if err != nil {
		return err
	}
	if err := h.protectUser(c, u.Name); err != nil {
		return err
	}
	if c.User != nil && c.User.ID == u.ID {
		return Errorf(http.StatusBadRequest, "InvalidArgument", "you cannot delete yourself")
	}
	if _, err := h.propose(c, &meta.DeleteUser{ID: u.ID}); err != nil {
		return err
	}
	h.audit(c, "user.delete", u.Name, "")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- keys ----

func (h *Handler) listKeys(c *Call) error {
	u, err := h.userByName(c)
	if err != nil {
		return err
	}
	keys, err := h.d.DB.ListAccessKeys(c.Ctx(), u.ID)
	if err != nil {
		return err
	}
	out := make([]AccessKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyOut(k, u.Name))
	}
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) listAllKeys(c *Call) error {
	keys, err := h.d.DB.ListAccessKeys(c.Ctx(), "")
	if err != nil {
		return err
	}
	users, _ := h.d.DB.ListUsers(c.Ctx())
	names := map[string]string{}
	for _, u := range users {
		names[u.ID] = u.Name
	}
	out := make([]AccessKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyOut(k, names[k.UserID]))
	}
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) createKey(c *Call) error {
	u, err := h.userByName(c)
	if err != nil {
		return err
	}
	var req CreateKeyRequest
	if c.R.ContentLength != 0 {
		if err := c.Decode(&req); err != nil {
			return err
		}
	}
	ak, sk := ids.AccessKeyID(), ids.SecretKey()
	now := h.d.Now()
	if _, err := h.propose(c, &meta.CreateAccessKey{AccessKeyID: ak, Secret: sk, UserID: u.ID, Label: req.Label, Now: now.UnixMilli()}); err != nil {
		return err
	}
	h.audit(c, "key.create", u.Name, ak)
	return c.JSON(http.StatusCreated, CreatedKey{
		AccessKey:       AccessKey{AccessKeyID: ak, User: u.Name, Label: req.Label, CreatedAt: now.UTC().Truncate(time.Millisecond)},
		SecretAccessKey: sk,
	})
}

func (h *Handler) updateKey(c *Call) error {
	var req UpdateKeyRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	id := c.Params["id"]
	if err := h.protectKey(c, id); err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.UpdateAccessKey{AccessKeyID: id, Disabled: req.Disabled}); err != nil {
		return err
	}
	h.audit(c, "key.update", id, map[bool]string{true: "disabled", false: "enabled"}[req.Disabled])
	cred, err := h.d.DB.LookupAccessKey(c.Ctx(), id)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, keyOut(cred.Key, cred.User.Name))
}

func (h *Handler) deleteKey(c *Call) error {
	id := c.Params["id"]
	if err := h.protectKey(c, id); err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.DeleteAccessKey{AccessKeyID: id}); err != nil {
		return err
	}
	h.audit(c, "key.delete", id, "")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- buckets ----

func (h *Handler) listBuckets(c *Call) error {
	bs, err := h.d.DB.ListBuckets(c.Ctx(), "")
	if err != nil {
		return err
	}
	out := make([]Bucket, 0, len(bs))
	for i := range bs {
		out = append(out, h.bucketOut(c.Ctx(), &bs[i], false))
	}
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) createBucket(c *Call) error {
	var req CreateBucketRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	if !api.ValidBucketName(req.Name) {
		return Errorf(http.StatusBadRequest, "InvalidBucketName", "bucket name %q is not valid", req.Name)
	}
	owner := ""
	if c.User != nil {
		owner = c.User.ID
	}
	if req.Owner != "" {
		u, err := h.d.DB.GetUserByName(c.Ctx(), req.Owner)
		if err != nil {
			return Errorf(http.StatusBadRequest, "NoSuchUser", "owner %q not found", req.Owner)
		}
		owner = u.ID
	}
	if _, err := h.propose(c, &meta.CreateBucket{Name: req.Name, Owner: owner, Now: h.d.Now().UnixMilli(), ObjectLock: req.ObjectLock}); err != nil {
		return err
	}
	if req.Versioning != "" && !req.ObjectLock {
		if _, err := h.propose(c, &meta.SetBucketVersioning{Name: req.Name, State: req.Versioning}); err != nil {
			return err
		}
	}
	h.audit(c, "bucket.create", req.Name, "")
	b, err := h.d.DB.GetBucket(c.Ctx(), req.Name)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, h.bucketOut(c.Ctx(), b, true))
}

func (h *Handler) bucketByName(c *Call) (*meta.Bucket, error) {
	b, err := h.d.DB.GetBucket(c.Ctx(), c.Params["name"])
	if err != nil {
		return nil, Errorf(http.StatusNotFound, "NoSuchBucket", "bucket %q not found", c.Params["name"])
	}
	return b, nil
}

func (h *Handler) getBucket(c *Call) error {
	b, err := h.bucketByName(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, h.bucketOut(c.Ctx(), b, true))
}

func (h *Handler) updateBucket(c *Call) error {
	b, err := h.bucketByName(c)
	if err != nil {
		return err
	}
	var req UpdateBucketRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	if req.Versioning != nil {
		if _, err := h.propose(c, &meta.SetBucketVersioning{Name: b.Name, State: *req.Versioning}); err != nil {
			return err
		}
	}
	if req.PublicRead != nil {
		p := ""
		if *req.PublicRead {
			p = policy.PublicReadPolicy(b.Name)
		}
		req.Policy = &p
	}
	if req.Policy != nil {
		if *req.Policy != "" {
			if _, err := policy.Parse([]byte(*req.Policy), b.Name); err != nil {
				return Errorf(http.StatusBadRequest, "MalformedPolicy", "%v", err)
			}
		}
		if _, err := h.propose(c, &meta.SetBucketPolicy{Name: b.Name, Policy: *req.Policy}); err != nil {
			return err
		}
	}
	if req.CORS != nil {
		raw := *req.CORS
		if raw != "" {
			var cfg s3xml.CORSConfiguration
			if err := s3xml.Unmarshal(strings.NewReader(raw), &cfg, s3xml.DefaultLimit); err != nil {
				return Errorf(http.StatusBadRequest, "MalformedXML", "%v", err)
			}
			if err := api.ValidateCORS(&cfg); err != nil {
				return Errorf(http.StatusBadRequest, "InvalidRequest", "%v", err)
			}
			b, _ := s3xml.Marshal(&cfg)
			raw = string(b)
		}
		if _, err := h.propose(c, &meta.SetBucketCORS{Name: b.Name, CORS: raw}); err != nil {
			return err
		}
	}
	if req.Lifecycle != nil {
		raw := *req.Lifecycle
		if raw != "" {
			var cfg s3xml.LifecycleConfiguration
			if err := s3xml.Unmarshal(strings.NewReader(raw), &cfg, s3xml.DefaultLimit); err != nil {
				return Errorf(http.StatusBadRequest, "MalformedXML", "%v", err)
			}
			if err := api.ValidateLifecycle(&cfg); err != nil {
				return Errorf(http.StatusBadRequest, "InvalidRequest", "%v", err)
			}
			b, _ := s3xml.Marshal(&cfg)
			raw = string(b)
		}
		if _, err := h.propose(c, &meta.SetBucketLifecycle{Name: b.Name, Lifecycle: raw}); err != nil {
			return err
		}
	}
	h.audit(c, "bucket.update", b.Name, "")
	b, _ = h.d.DB.GetBucket(c.Ctx(), b.Name)
	return c.JSON(http.StatusOK, h.bucketOut(c.Ctx(), b, true))
}

func (h *Handler) deleteBucket(c *Call) error {
	b, err := h.bucketByName(c)
	if err != nil {
		return err
	}
	if force := c.R.URL.Query().Get("force"); force == "true" || force == "1" {
		n, err := h.purgeBucket(c, b.Name)
		if err != nil {
			return err
		}
		h.audit(c, "bucket.purge", b.Name, strconv.Itoa(n)+" versions removed")
	}
	if _, err := h.propose(c, &meta.DeleteBucket{Name: b.Name, Now: h.d.Now().UnixMilli()}); err != nil {
		var me *meta.Error
		if errors.As(err, &me) && me.Code == meta.CodeBucketNotEmpty {
			v, m, _ := h.d.DB.BucketVersionStats(c.Ctx(), b.Name)
			return Errorf(http.StatusConflict, "BucketNotEmpty", "bucket %q still holds %d version(s), %d of them delete markers; delete them or use force=true to remove the bucket with all its contents", b.Name, v, m)
		}
		return err
	}
	h.audit(c, "bucket.delete", b.Name, "")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

// purgeBucket removes every version and delete marker of a bucket. Versions
// under a compliance retention that has not expired, or under legal hold,
// cannot be removed and make the whole purge fail before anything is deleted.
func (h *Handler) purgeBucket(c *Call, bucket string) (int, error) {
	var blocked []string
	var all []meta.ObjectVersion
	opts := meta.VersionListOpts{Max: 1000}
	now := h.d.Now()
	for {
		res, err := h.d.DB.ListVersions(c.Ctx(), bucket, opts)
		if err != nil {
			return 0, err
		}
		for _, v := range res.Versions {
			if v.Metadata["x-amz-object-lock-legal-hold"] == "ON" {
				blocked = append(blocked, v.Key+" (legal hold)")
			} else if v.Metadata["x-amz-object-lock-mode"] == "COMPLIANCE" {
				if until, err := time.Parse(time.RFC3339, v.Metadata["x-amz-object-lock-retain-until-date"]); err == nil && until.After(now) {
					blocked = append(blocked, v.Key+" (compliance retention until "+until.UTC().Format(time.RFC3339)+")")
				}
			}
			all = append(all, v)
		}
		if !res.IsTruncated {
			break
		}
		opts.KeyMarker, opts.VersionIDMarker = res.NextKeyMarker, res.NextVersionIDMarker
	}
	if len(blocked) > 0 {
		if len(blocked) > 5 {
			blocked = append(blocked[:5], "...")
		}
		return 0, Errorf(http.StatusConflict, "ObjectLocked", "cannot purge %q: %s", bucket, strings.Join(blocked, "; "))
	}
	for _, v := range all {
		if _, err := h.propose(c, &meta.DeleteObject{Bucket: bucket, Key: v.Key, VersionID: v.VersionID, Now: now.UnixMilli()}); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}

// ---- audit, settings, config ----

func (h *Handler) listAudit(c *Call) error {
	q := c.R.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	entries, err := h.d.DB.ListAudit(c.Ctx(), limit, before)
	if err != nil {
		return err
	}
	out := make([]AuditEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, AuditEntry{ID: e.ID, At: e.At, Actor: e.Actor, Action: e.Action, Target: e.Target, Detail: e.Detail})
	}
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) listSettings(c *Call) error {
	s, err := h.d.DB.AllSettings(c.Ctx())
	if err != nil {
		return err
	}
	out := []Setting{}
	for k, d := range KnownSettings {
		out = append(out, Setting{Key: k, Value: s[k], Description: d})
	}
	for k, v := range s {
		if _, known := KnownSettings[k]; !known && !strings.HasPrefix(k, "bucket.") && !strings.HasPrefix(k, "replication.") && !strings.HasPrefix(k, "ui.") && !strings.HasPrefix(k, "cellar.") && !strings.HasPrefix(k, "server.") {
			out = append(out, Setting{Key: k, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) putSetting(c *Call) error {
	key := c.Params["key"]
	var req SettingRequest
	if err := c.Decode(&req); err != nil {
		return err
	}
	if err := validateSetting(key, req.Value); err != nil {
		return err
	}
	if err := h.settingWritable(c, key, req.Value); err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.SetSetting{Key: key, Value: req.Value}); err != nil {
		return err
	}
	h.audit(c, "setting.set", key, req.Value)
	return c.JSON(http.StatusOK, map[string]string{"key": key, "value": req.Value})
}

func validateSetting(key, value string) error {
	if value == "" {
		return nil
	}
	if strings.HasPrefix(key, "webhook.") && !strings.HasSuffix(key, ".secret") {
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Errorf(http.StatusBadRequest, "InvalidArgument", "%s must be an http(s) URL", key)
		}
		return nil
	}
	switch key {
	case SettingUploadMaxAge, SettingGCGrace:
		if _, err := time.ParseDuration(value); err != nil {
			return Errorf(http.StatusBadRequest, "InvalidArgument", "%s must be a duration such as 24h", key)
		}
	}
	return nil
}

func (h *Handler) getConfig(c *Call) error {
	cfg := h.d.Cfg()
	cfg.TLS.DNS.Settings = redact(cfg.TLS.DNS.Settings)
	b, err := config.Marshal(cfg)
	if err != nil {
		return err
	}
	out := ConfigView{Path: h.d.ConfigPath, TOML: string(b), PendingRestart: []string{}}
	if h.d.PendingRestart != nil {
		if p := h.d.PendingRestart(); p != nil {
			out.PendingRestart = p
		}
	}
	return c.JSON(http.StatusOK, out)
}

func redact(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for k := range m {
		out[k] = "***"
	}
	return out
}

func (h *Handler) reloadConfig(c *Call) error {
	if h.d.Reload == nil {
		return Errorf(http.StatusBadRequest, "InvalidRequest", "no configuration file to reload")
	}
	if err := h.d.Reload(); err != nil {
		return Errorf(http.StatusBadRequest, "InvalidRequest", "reload failed: %v", err)
	}
	h.audit(c, "config.reload", h.d.ConfigPath, "")
	return h.getConfig(c)
}

// openapi renders a minimal OpenAPI 3 document from the route table.
func (h *Handler) openapi(c *Call) error {
	paths := map[string]map[string]any{}
	for _, rt := range h.routes {
		p := Prefix + rt.Path
		if paths[p] == nil {
			paths[p] = map[string]any{}
		}
		op := map[string]any{"summary": rt.Summary, "responses": map[string]any{"200": map[string]any{"description": "OK"}}}
		if rt.Admin {
			op["x-requires-admin"] = true
		}
		var params []map[string]any
		for _, seg := range strings.Split(rt.Path, "/") {
			if strings.HasPrefix(seg, "{") {
				params = append(params, map[string]any{"name": strings.Trim(seg, "{}"), "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
			}
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		paths[p][strings.ToLower(rt.Method)] = op
	}
	doc := map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": "hogshead admin API", "version": h.d.Version},
		"paths":   paths,
	}
	return c.JSON(http.StatusOK, doc)
}
