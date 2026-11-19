package meta

import (
	"strings"
	"time"
)

// ChunkRef identifies a stored chunk by content hash and records its size.
type ChunkRef struct {
	Hash string `json:"h"` // hex SHA-256 of plaintext
	Size int64  `json:"s"`
}

// Checksums holds the optional AWS additional checksums of an object or part,
// base64 encoded as AWS transmits them.
type Checksums struct {
	CRC32     string `json:"crc32,omitempty"`
	CRC32C    string `json:"crc32c,omitempty"`
	SHA1      string `json:"sha1,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	CRC64NVME string `json:"crc64nvme,omitempty"`
	// Type is FULL_OBJECT or COMPOSITE for multipart results.
	Type string `json:"type,omitempty"`
}

// Versioning states for a bucket.
const (
	VersioningOff       = ""
	VersioningEnabled   = "Enabled"
	VersioningSuspended = "Suspended"
)

// NullVersionID is the version id of objects in unversioned or suspended buckets.
const NullVersionID = "null"

// Bucket is bucket metadata.
type Bucket struct {
	Name             string
	Owner            string
	CreatedAt        time.Time
	Versioning       string
	Policy           string // raw JSON, empty when unset
	CORS             string // raw XML, empty when unset
	Lifecycle        string // raw XML, empty when unset
	WriteConsistency string // retained column, unused
	PartitionReads   string // retained column, unused
	ObjectLock       bool
}

// ObjectVersion is one version row.
type ObjectVersion struct {
	Bucket       string
	Key          string
	VersionID    string
	Seq          int64
	IsLatest     bool
	DeleteMarker bool
	Size         int64
	ETag         string
	ContentType  string
	Metadata     map[string]string // user metadata and selected system headers
	Chunks       []ChunkRef
	Checksums    Checksums
	StorageClass string
	Owner        string
	Created      time.Time
}

// Upload is an in-progress multipart upload.
type Upload struct {
	ID           string
	Bucket       string
	Key          string
	Owner        string
	ContentType  string
	Metadata     map[string]string
	ChecksumAlgo string
	ChecksumType string
	Created      time.Time
	StorageClass string
}

// Part is one uploaded part.
type Part struct {
	UploadID  string
	Number    int
	Size      int64
	ETag      string
	Chunks    []ChunkRef
	Checksums Checksums
	Created   time.Time
}

// Roles.
const (
	RoleAdmin     = "admin"
	RoleReadWrite = "read-write"
	RoleReadOnly  = "read-only"
)

// User is an identity that owns access keys.
type User struct {
	ID        string
	Name      string
	Role      string
	Buckets   []string // empty means all buckets
	Disabled  bool
	CreatedAt time.Time
	// PasswordHash is the bcrypt hash for web UI login; empty when the user
	// has no password. Never serialised by the admin API.
	PasswordHash string
}

// AccessKey is a SigV4 credential.
type AccessKey struct {
	AccessKeyID string
	Secret      string
	UserID      string
	Label       string
	Disabled    bool
	CreatedAt   time.Time
}

// Replication log operations.
const (
	ReplicationPut    = "put"    // a new object version
	ReplicationMarker = "marker" // a delete marker
	ReplicationDelete = "delete" // a version removed (including an unversioned overwrite)
	// ReplicationMeta is a change to one version's metadata without new
	// content: retention, legal hold, tags or ACL. Replayed to the same
	// version on the other side.
	ReplicationMeta = "meta"
	// ReplicationBucket is a bucket configuration change; Key names the kind:
	// create, versioning, policy, cors, lifecycle, tagging, lock, notification.
	ReplicationBucket = "bucket"
	// ReplicationUser, ReplicationKey and ReplicationSetting are server-level
	// changes (Bucket ""), replayed only to targets that sync configuration.
	ReplicationUser    = "user"
	ReplicationKey     = "key"
	ReplicationSetting = "setting"
)

// ReplicationEntry is one change recorded for the replication worker.
type ReplicationEntry struct {
	Seq       int64
	Bucket    string // "" for server-level entries (users, keys, settings)
	Key       string // object key, or the config kind / user name / key id / setting key
	VersionID string // object version, or the owning user name for key entries
	Op        string
	At        time.Time
	// Origin is the server id the change was replicated from; empty for
	// changes made directly on this server. Replicated changes are applied
	// but never forwarded, which keeps meshes loop-free.
	Origin string
}

// AuditEntry records an administrative or authentication event.
type AuditEntry struct {
	ID     int64
	At     time.Time
	Actor  string
	Action string
	Target string
	Detail string
}

// Chunk is a chunk reference-count row.
type Chunk struct {
	Hash    string
	Size    int64
	Refs    int64
	UnrefAt time.Time // zero when referenced
}

// ReplicableSetting reports whether a server setting is mirrored to targets
// that sync configuration. Replication state, the web UI secret, the server
// identity and per-bucket settings stay local.
func ReplicableSetting(key string) bool {
	for _, p := range []string{"replication.", "bucket.", "ui.", "server."} {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return true
}
