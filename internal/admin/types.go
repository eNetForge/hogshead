// Package admin implements the JSON management API. The CLI and the web UI
// are clients of it; nothing is manageable any other way.
package admin

import "time"

// Prefix is the URL prefix of the admin API.
const Prefix = "/_admin/v1"

// ErrorBody is the JSON error envelope.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Status describes the server.
type Status struct {
	Version        string            `json:"version"`
	NodeName       string            `json:"node_name"` // the configured name, or the hostname
	ID             string            `json:"id,omitempty"`
	Uptime         string            `json:"uptime"`
	StartedAt      time.Time         `json:"started_at"`
	Stats          Stats             `json:"stats"`
	Workers        WorkerStats       `json:"workers"`
	PendingRestart []string          `json:"pending_restart"`
	Settings       map[string]string `json:"settings"`
	Listen         []string          `json:"listen"`
	TLSMode        string            `json:"tls_mode"`
}

// Stats are storage counters.
type Stats struct {
	Buckets int64 `json:"buckets"`
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
	Uploads int64 `json:"uploads"`
	Chunks  int64 `json:"chunks"`
}

// WorkerStats mirror worker.Stats.
type WorkerStats struct {
	LastGC          time.Time `json:"last_gc"`
	ChunksReleased  int64     `json:"chunks_released"`
	OrphansRemoved  int64     `json:"orphans_removed"`
	BytesReclaimed  int64     `json:"bytes_reclaimed"`
	LastLifecycle   time.Time `json:"last_lifecycle"`
	ObjectsExpired  int64     `json:"objects_expired"`
	VersionsExpired int64     `json:"versions_expired"`
	MarkersRemoved  int64     `json:"markers_removed"`
	UploadsAborted  int64     `json:"uploads_aborted"`
	LastError       string    `json:"last_error,omitempty"`
}

// User is a user as returned by the API.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Buckets   []string  `json:"buckets"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
	Keys      int       `json:"keys,omitempty"`
	// HasPassword reports whether the user can log in to the web UI with a password.
	HasPassword bool `json:"has_password"`
}

// UserImportRequest creates or replaces a user wholesale (config sync).
type UserImportRequest struct {
	Role         string   `json:"role"`
	Buckets      []string `json:"buckets"`
	Disabled     bool     `json:"disabled"`
	PasswordHash string   `json:"password_hash,omitempty"`
}

// KeyImportRequest creates or replaces an access key with a known secret.
type KeyImportRequest struct {
	Secret   string `json:"secret"`
	Label    string `json:"label,omitempty"`
	Disabled bool   `json:"disabled"`
}

// TransferRequest starts an import or export job.
type TransferRequest struct {
	Direction    string `json:"direction"` // import | export
	Bucket       string `json:"bucket"`    // local bucket
	Prefix       string `json:"prefix,omitempty"`
	Endpoint     string `json:"endpoint"`
	RemoteBucket string `json:"remote_bucket"`
	RemotePrefix string `json:"remote_prefix,omitempty"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	Region       string `json:"region,omitempty"`
	PathStyle    *bool  `json:"path_style,omitempty"` // default true
	Overwrite    string `json:"overwrite,omitempty"`  // if-changed | always | never
	DryRun       bool   `json:"dry_run,omitempty"`
	Concurrency  int    `json:"concurrency,omitempty"`
}

// PasswordRequest sets or clears a user's web UI password.
type PasswordRequest struct {
	Password string `json:"password"` // empty removes password login
}

// CreateUserRequest creates a user.
type CreateUserRequest struct {
	Name    string   `json:"name"`
	Role    string   `json:"role"`
	Buckets []string `json:"buckets,omitempty"`
}

// UpdateUserRequest changes a user. Nil fields are left alone.
type UpdateUserRequest struct {
	Role     *string   `json:"role,omitempty"`
	Buckets  *[]string `json:"buckets,omitempty"`
	Disabled *bool     `json:"disabled,omitempty"`
}

// AccessKey is a key without its secret.
type AccessKey struct {
	AccessKeyID string    `json:"access_key_id"`
	User        string    `json:"user"`
	Label       string    `json:"label"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateKeyRequest creates a key.
type CreateKeyRequest struct {
	Label string `json:"label,omitempty"`
}

// CreatedKey is returned once, with the secret.
type CreatedKey struct {
	AccessKey
	SecretAccessKey string `json:"secret_access_key"`
}

// UpdateKeyRequest enables or disables a key.
type UpdateKeyRequest struct {
	Disabled bool `json:"disabled"`
}

// Bucket is a bucket as returned by the API.
type Bucket struct {
	Name       string    `json:"name"`
	Owner      string    `json:"owner"`
	OwnerName  string    `json:"owner_name"`
	CreatedAt  time.Time `json:"created_at"`
	Versioning string    `json:"versioning"`
	ObjectLock bool      `json:"object_lock"`
	Policy     string    `json:"policy,omitempty"`
	CORS       string    `json:"cors,omitempty"`
	Lifecycle  string    `json:"lifecycle,omitempty"`
	Objects    int64     `json:"objects"`
	Bytes      int64     `json:"bytes"`
	// Versions counts every stored version including non-current ones and
	// delete markers; DeleteMarkers counts the markers. S3 only lets an
	// entirely empty bucket (Versions == 0) be deleted.
	Versions      int64 `json:"versions"`
	DeleteMarkers int64 `json:"delete_markers"`
}

// CreateBucketRequest creates a bucket.
type CreateBucketRequest struct {
	Name       string `json:"name"`
	Versioning string `json:"versioning,omitempty"`
	ObjectLock bool   `json:"object_lock,omitempty"`
	Owner      string `json:"owner,omitempty"` // user name; defaults to caller
}

// UpdateBucketRequest changes bucket settings. Nil leaves a field alone; an
// empty string clears policy, CORS and lifecycle.
type UpdateBucketRequest struct {
	Versioning *string `json:"versioning,omitempty"`
	Policy     *string `json:"policy,omitempty"`
	CORS       *string `json:"cors,omitempty"`
	Lifecycle  *string `json:"lifecycle,omitempty"`
	PublicRead *bool   `json:"public_read,omitempty"`
}

// AuditEntry is one audit record.
type AuditEntry struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Detail string    `json:"detail,omitempty"`
}

// Setting is a server setting with its description.
type Setting struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// SettingRequest sets a server setting.
type SettingRequest struct {
	Value string `json:"value"`
}

// ConfigView is the node-local config with secrets removed.
type ConfigView struct {
	Path           string   `json:"path"`
	TOML           string   `json:"toml"`
	PendingRestart []string `json:"pending_restart"`
}

// Me describes the calling identity.
type Me struct {
	User User   `json:"user"`
	Via  string `json:"via"` // "socket", "sigv4" or "session"
}

// Known server settings and their defaults.
const (
	SettingUploadMaxAge = "uploads.max_age"      // e.g. "168h", default "" (never)
	SettingGCGrace      = "gc.grace"             // default 24h
	SettingUsageKeep    = "usage.retention_days" // default 90
)

// KnownSettings lists settings the API accepts with a description.
var KnownSettings = map[string]string{
	SettingUsageKeep:    "Days of per-bucket/user/key usage counters to keep (default 90)",
	SettingUploadMaxAge: "Abort multipart uploads older than this duration, e.g. 168h. Empty disables.",
	SettingGCGrace:      "How long unreferenced chunks are kept before deletion, e.g. 24h.",
	"webhook.<name>":    "Webhook URL for bucket notifications; reference it as arn:hogshead:webhook:::<name>. Optional webhook.<name>.secret signs payloads (X-Hogshead-Signature).",
}
