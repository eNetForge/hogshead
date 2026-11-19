package meta

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// Command is a metadata mutation. Commands are serialisable and applied by a
// single deterministic function, so they must carry everything they need
// (timestamps, generated ids) and must not read the clock or randomness.
type Command interface {
	name() string
}

type envelope struct {
	Name    string          `json:"n"`
	Payload json.RawMessage `json:"p"`
}

var registry = map[string]reflect.Type{}

func register(cmds ...Command) {
	for _, c := range cmds {
		registry[c.name()] = reflect.TypeOf(c).Elem()
	}
}

// Encode serialises a command for the log.
func Encode(c Command) ([]byte, error) {
	p, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{Name: c.name(), Payload: p})
}

// Decode parses a command from the log.
func Decode(b []byte) (Command, error) {
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("decode command envelope: %w", err)
	}
	t, ok := registry[env.Name]
	if !ok {
		return nil, fmt.Errorf("unknown command %q", env.Name)
	}
	v := reflect.New(t).Interface().(Command)
	if err := json.Unmarshal(env.Payload, v); err != nil {
		return nil, fmt.Errorf("decode command %s: %w", env.Name, err)
	}
	return v, nil
}

// Result is what Apply returns for a command. Err is a stable code so that
// the result is identical on every node.
type Result struct {
	Err   Code            `json:"e,omitempty"`
	Msg   string          `json:"m,omitempty"`
	Value json.RawMessage `json:"v,omitempty"`
}

// Error converts a Result into an error, or nil.
func (r Result) Error() error {
	if r.Err == "" {
		return nil
	}
	return &Error{Code: r.Err, Msg: r.Msg}
}

// Decode unmarshals the result value into v.
func (r Result) Decode(v any) error {
	if len(r.Value) == 0 {
		return nil
	}
	return json.Unmarshal(r.Value, v)
}

// ---- bucket commands ----

// CreateBucket creates a bucket.
type CreateBucket struct {
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Now        int64  `json:"now"` // unix milliseconds
	ObjectLock bool   `json:"lock,omitempty"`
}

// DeleteBucket removes an empty bucket, discarding in-progress uploads.
type DeleteBucket struct {
	Name string `json:"name"`
	Now  int64  `json:"now,omitempty"`
}

// SetBucketVersioning sets the versioning state.
type SetBucketVersioning struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// SetBucketPolicy sets or clears (empty) the bucket policy JSON.
type SetBucketPolicy struct {
	Name   string `json:"name"`
	Policy string `json:"policy"`
}

// SetBucketCORS sets or clears the CORS XML.
type SetBucketCORS struct {
	Name string `json:"name"`
	CORS string `json:"cors"`
}

// SetBucketLifecycle sets or clears the lifecycle XML.
type SetBucketLifecycle struct {
	Name      string `json:"name"`
	Lifecycle string `json:"lifecycle"`
}

// ---- object commands ----

// PutObjectVersion records a new object version whose chunks are already
// durable. VersionID must be NullVersionID when the bucket is not versioned;
// Apply corrects it based on bucket state and returns the effective id.
type PutObjectVersion struct {
	Bucket       string            `json:"bucket"`
	Key          string            `json:"key"`
	VersionID    string            `json:"vid"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	ContentType  string            `json:"ct,omitempty"`
	Metadata     map[string]string `json:"meta,omitempty"`
	Chunks       []ChunkRef        `json:"chunks"`
	Checksums    Checksums         `json:"cks"`
	StorageClass string            `json:"sc,omitempty"`
	Owner        string            `json:"owner,omitempty"`
	Now          int64             `json:"now"`
	// Conditional write support.
	IfNoneMatchAny  bool   `json:"inm,omitempty"`  // If-None-Match: *
	IfMatchETag     string `json:"im,omitempty"`   // If-Match: etag
	IfNoneMatchETag string `json:"inme,omitempty"` // If-None-Match: etag
}

// PutObjectResult is returned by PutObjectVersion.
type PutObjectResult struct {
	VersionID string `json:"vid"`
}

// UpdateObjectMeta replaces or removes entries in a version's metadata map
// without changing its data. Used for tagging, retention and legal hold.
type UpdateObjectMeta struct {
	Bucket    string            `json:"bucket"`
	Key       string            `json:"key"`
	VersionID string            `json:"vid,omitempty"` // empty = latest
	Set       map[string]string `json:"set,omitempty"`
	Remove    []string          `json:"rm,omitempty"`
}

// DeleteObject deletes an object or specific version. With an empty
// VersionID on a versioned bucket it inserts a delete marker.
type DeleteObject struct {
	Bucket          string `json:"bucket"`
	Key             string `json:"key"`
	VersionID       string `json:"vid,omitempty"`
	MarkerVersionID string `json:"mvid,omitempty"` // id to use if a delete marker is created
	Now             int64  `json:"now"`
}

// DeleteObjectResult is returned by DeleteObject.
type DeleteObjectResult struct {
	DeleteMarker bool   `json:"dm,omitempty"`
	VersionID    string `json:"vid,omitempty"`
	Existed      bool   `json:"ex,omitempty"`
}

// DeleteObjects deletes many keys in one log entry.
type DeleteObjects struct {
	Bucket string         `json:"bucket"`
	Items  []DeleteObject `json:"items"`
	Now    int64          `json:"now"`
}

// DeleteObjectsResult carries one result per item, in order.
type DeleteObjectsResult struct {
	Results []DeleteObjectItemResult `json:"r"`
}

// DeleteObjectItemResult is one DeleteObjects entry outcome.
type DeleteObjectItemResult struct {
	DeleteObjectResult
	Err Code   `json:"e,omitempty"`
	Msg string `json:"m,omitempty"`
}

// ---- multipart commands ----

// CreateUpload starts a multipart upload.
type CreateUpload struct {
	UploadID     string            `json:"id"`
	Bucket       string            `json:"bucket"`
	Key          string            `json:"key"`
	Owner        string            `json:"owner,omitempty"`
	ContentType  string            `json:"ct,omitempty"`
	Metadata     map[string]string `json:"meta,omitempty"`
	ChecksumAlgo string            `json:"ca,omitempty"`
	ChecksumType string            `json:"ctp,omitempty"`
	StorageClass string            `json:"sc,omitempty"`
	Now          int64             `json:"now"`
}

// PutPart records an uploaded part whose chunks are durable.
type PutPart struct {
	UploadID  string     `json:"id"`
	Number    int        `json:"n"`
	Size      int64      `json:"size"`
	ETag      string     `json:"etag"`
	Chunks    []ChunkRef `json:"chunks"`
	Checksums Checksums  `json:"cks"`
	Now       int64      `json:"now"`
}

// CompleteUpload assembles parts into an object version.
type CompleteUpload struct {
	UploadID  string         `json:"id"`
	VersionID string         `json:"vid"`
	Parts     []CompletePart `json:"parts"`
	Now       int64          `json:"now"`
	// Precomputed by the coordinator from the parts it validated; Apply
	// re-validates part numbers and ETags against stored parts.
	ETag           string    `json:"etag"`
	Checksums      Checksums `json:"cks"`
	IfNoneMatchAny bool      `json:"inm,omitempty"`
	// ExtraMetadata is merged into the upload's metadata (part sizes etc).
	ExtraMetadata map[string]string `json:"xmeta,omitempty"`
}

// CompletePart is a (number, etag) pair from the client.
type CompletePart struct {
	Number int    `json:"n"`
	ETag   string `json:"etag"`
}

// CompleteUploadResult is returned by CompleteUpload.
type CompleteUploadResult struct {
	VersionID string `json:"vid"`
	ETag      string `json:"etag"`
	Size      int64  `json:"size"`
}

// PurgeCompletedUploads forgets completed-upload records older than Before
// (unix ms); they exist only to make CompleteMultipartUpload idempotent.
type PurgeCompletedUploads struct {
	Before int64 `json:"before"`
}

// AbortUpload discards an upload and its parts.
type AbortUpload struct {
	UploadID string `json:"id"`
	Now      int64  `json:"now"`
}

// ---- identity commands ----

// CreateUser creates a user.
type CreateUser struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Role    string   `json:"role"`
	Buckets []string `json:"buckets,omitempty"`
	Now     int64    `json:"now"`
}

// UpdateUser changes role, bucket scope or disabled flag.
type UpdateUser struct {
	ID       string    `json:"id"`
	Role     *string   `json:"role,omitempty"`
	Buckets  *[]string `json:"buckets,omitempty"`
	Disabled *bool     `json:"disabled,omitempty"`
}

// SetUserPassword stores a bcrypt hash for web UI login; empty removes it.
type SetUserPassword struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// DeleteUser removes a user and its keys.
type DeleteUser struct {
	ID string `json:"id"`
}

// CreateAccessKey adds a key to a user.
type CreateAccessKey struct {
	AccessKeyID string `json:"ak"`
	Secret      string `json:"sk"`
	UserID      string `json:"user"`
	Label       string `json:"label,omitempty"`
	Now         int64  `json:"now"`
}

// UpdateAccessKey enables or disables a key.
type UpdateAccessKey struct {
	AccessKeyID string `json:"ak"`
	Disabled    bool   `json:"disabled"`
}

// DeleteAccessKey revokes a key.
type DeleteAccessKey struct {
	AccessKeyID string `json:"ak"`
}

// ---- settings commands ----

// SetSetting writes a server setting; empty value deletes it.
type SetSetting struct {
	Key   string `json:"k"`
	Value string `json:"v"`
}

// PutCert stores a certificate-store entry (used by certmagic storage).
type PutCert struct {
	Key   string `json:"k"`
	Value []byte `json:"v"`
	Now   int64  `json:"now"`
}

// DeleteCert removes a certificate-store entry or prefix.
type DeleteCert struct {
	Key    string `json:"k"`
	Prefix bool   `json:"prefix,omitempty"`
}

// Audit appends an audit log entry.
type Audit struct {
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
	Now    int64  `json:"now"`
}

// PutTransfer creates or updates a transfer job record (JSON in Data).
type PutTransfer struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Data    string `json:"data"`
}

// DeleteTransfer removes a transfer job record.
type DeleteTransfer struct {
	ID string `json:"id"`
}

// UsageRow is one hourly counter line.
type UsageRow struct {
	Hour      int64  `json:"h"` // unix seconds, truncated to the hour
	Bucket    string `json:"b"`
	User      string `json:"u"`
	AccessKey string `json:"k"`
	Class     string `json:"c"`           // read, write, delete, list, bucket, other
	Member    string `json:"m,omitempty"` // set by cellar-wide queries grouped by member; never stored
	Requests  int64  `json:"n"`
	Errors    int64  `json:"e"`
	BytesIn   int64  `json:"i"`
	BytesOut  int64  `json:"o"`
}

// AddUsage adds counters to the usage table (upsert, summed).
type AddUsage struct {
	Rows []UsageRow `json:"rows"`
}

// ErrorRow is one failed request, stored as JSON.
type ErrorRow struct {
	At   int64  `json:"at"` // unix milliseconds
	Data string `json:"d"`
}

// AddErrors appends failed requests and keeps only the newest Keep rows.
type AddErrors struct {
	Rows []ErrorRow `json:"rows"`
	Keep int        `json:"keep"`
}

// PruneUsage drops usage rows older than Before (unix seconds).
type PruneUsage struct {
	Before int64 `json:"before"`
}

// WithOrigin wraps a command that was replicated from another server so the
// resulting log entries carry that server's id and are not forwarded again.
type WithOrigin struct {
	Origin  string          `json:"o"`
	Command json.RawMessage `json:"c"`
}

// Wrap encodes cmd inside a WithOrigin.
func Wrap(origin string, cmd Command) (Command, error) {
	raw, err := Encode(cmd)
	if err != nil {
		return nil, err
	}
	return &WithOrigin{Origin: origin, Command: raw}, nil
}

// ReleaseChunks marks chunk rows as deleted after the GC removed the files.
type ReleaseChunks struct {
	Hashes []string `json:"h"`
}

// TrimReplicationLog deletes replication log rows at or below Seq.
type TrimReplicationLog struct {
	Seq int64 `json:"seq"`
}

// Batch applies several commands atomically; stops at the first error.
type Batch struct {
	Commands []json.RawMessage `json:"cmds"`
}

func (*CreateBucket) name() string          { return "bucket.create" }
func (*DeleteBucket) name() string          { return "bucket.delete" }
func (*SetBucketVersioning) name() string   { return "bucket.versioning" }
func (*SetBucketPolicy) name() string       { return "bucket.policy" }
func (*SetBucketCORS) name() string         { return "bucket.cors" }
func (*SetBucketLifecycle) name() string    { return "bucket.lifecycle" }
func (*PutObjectVersion) name() string      { return "object.put" }
func (*DeleteObject) name() string          { return "object.delete" }
func (*UpdateObjectMeta) name() string      { return "object.meta" }
func (*DeleteObjects) name() string         { return "object.deletemany" }
func (*CreateUpload) name() string          { return "upload.create" }
func (*PutPart) name() string               { return "upload.part" }
func (*CompleteUpload) name() string        { return "upload.complete" }
func (*AbortUpload) name() string           { return "upload.abort" }
func (*PurgeCompletedUploads) name() string { return "upload.purge" }
func (*CreateUser) name() string            { return "user.create" }
func (*UpdateUser) name() string            { return "user.update" }
func (*DeleteUser) name() string            { return "user.delete" }
func (*SetUserPassword) name() string       { return "user.password" }
func (*PutTransfer) name() string           { return "transfer.put" }
func (*WithOrigin) name() string            { return "origin" }
func (*AddUsage) name() string              { return "usage.add" }
func (*PruneUsage) name() string            { return "usage.prune" }
func (*AddErrors) name() string             { return "errors.add" }
func (*DeleteTransfer) name() string        { return "transfer.delete" }
func (*CreateAccessKey) name() string       { return "key.create" }
func (*UpdateAccessKey) name() string       { return "key.update" }
func (*DeleteAccessKey) name() string       { return "key.delete" }
func (*SetSetting) name() string            { return "setting.set" }
func (*PutCert) name() string               { return "cert.put" }
func (*DeleteCert) name() string            { return "cert.delete" }
func (*Audit) name() string                 { return "audit" }
func (*ReleaseChunks) name() string         { return "chunks.release" }
func (*TrimReplicationLog) name() string    { return "replication.trim" }
func (*Batch) name() string                 { return "batch" }

func init() {
	register(
		&CreateBucket{}, &DeleteBucket{}, &SetBucketVersioning{}, &SetBucketPolicy{},
		&SetBucketCORS{}, &SetBucketLifecycle{},
		&PutObjectVersion{}, &DeleteObject{}, &DeleteObjects{}, &UpdateObjectMeta{},
		&CreateUpload{}, &PutPart{}, &CompleteUpload{}, &AbortUpload{}, &PurgeCompletedUploads{},
		&CreateUser{}, &UpdateUser{}, &DeleteUser{}, &SetUserPassword{},
		&CreateAccessKey{}, &UpdateAccessKey{}, &DeleteAccessKey{},
		&SetSetting{}, &PutCert{}, &DeleteCert{},
		&Audit{}, &ReleaseChunks{}, &TrimReplicationLog{}, &Batch{},
		&PutTransfer{}, &DeleteTransfer{}, &WithOrigin{}, &AddUsage{}, &PruneUsage{}, &AddErrors{},
	)
}
