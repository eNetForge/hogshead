package replication

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/s3client"
	"github.com/enetforge/hogshead/internal/store"
)

// Settings keys.
const (
	RulePrefix = "bucket.replication:"
	// GlobalKey holds the server-wide rule applied to every bucket.
	GlobalKey    = "replication.global"
	CursorPrefix = "replication.cursor:"
)

// Rule is a bucket's replication configuration: one or many standby
// targets, each pushed independently with its own cursor.
type Rule struct {
	Targets []Target `json:"targets"`
}

// Find returns the target with the given name.
func (r Rule) Find(name string) (int, bool) {
	for i, t := range r.Targets {
		if t.Name == name {
			return i, true
		}
	}
	return -1, false
}

// TargetStatus is what the admin API reports per bucket and target.
type TargetStatus struct {
	Bucket    string    `json:"bucket"`
	Target    Target    `json:"target"`
	Cursor    int64     `json:"cursor"`
	Head      int64     `json:"head"`
	Lag       int64     `json:"lag"` // entries not yet pushed
	LastSync  time.Time `json:"last_sync,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Pushed    int64     `json:"pushed"`
}

// RuleStatus is kept as an alias for callers that still use the old name.
type RuleStatus = TargetStatus

// Deps are the worker's collaborators.
type Deps struct {
	DB      *meta.DB
	Propose func(ctx context.Context, cmd meta.Command) (meta.Result, error)
	Chunks  store.ChunkStore
	Log     *slog.Logger
	Now     func() time.Time
	// ServerID identifies this server; it tags pushed changes so a mesh of
	// servers replicating to each other never forwards a change twice.
	ServerID func() string
	// WriteAck returns the acknowledgement level for writes: "local" (or
	// empty) acknowledges after the local commit, "one" after at least one
	// cellar member confirmed, "all" after every enabled cellar member did.
	WriteAck func(ctx context.Context) string
	// NewClient lets tests swap the destination client.
	NewClient func(t Target) Pusher
}

// Pusher is the destination abstraction; *s3client.Client satisfies it.
type Pusher interface {
	PutObject(ctx context.Context, key string, body io.Reader, size int64, headers map[string]string) error
	DeleteObject(ctx context.Context, key string) error
	// DeleteObjectVersion removes one version (or the current one when
	// versionID is empty), passing headers such as the source marker id.
	DeleteObjectVersion(ctx context.Context, key, versionID string, headers map[string]string) error
	// EnsureBucket makes sure the destination bucket exists, creating it
	// (with lock or versioning when asked) if the destination reports it
	// missing, and reports whether it did.
	EnsureBucket(ctx context.Context, o s3client.BucketOptions) (bool, error)
	PutBucketConfig(ctx context.Context, subresource, body string) error
	DeleteBucketConfig(ctx context.Context, subresource string) error
	// AdminJSON calls the hogshead admin API of the target (config sync).
	AdminJSON(ctx context.Context, method, path string, in, out any) error
	// HeadObjectVersion reports whether a version exists (backfill checks).
	HeadObjectVersion(ctx context.Context, key, versionID string) (*s3client.ObjectInfo, error)
	// PutObjectSubresource and DeleteObjectSubresource replay retention,
	// legal hold and tag changes to one version.
	PutObjectSubresource(ctx context.Context, key, versionID, subresource, body string, headers map[string]string) error
	DeleteObjectSubresource(ctx context.Context, key, versionID, subresource string) error
	// PurgeBucket removes a bucket and all its versions (cellar bucket deletion).
	PurgeBucket(ctx context.Context) error
}

// Worker runs the replication loop on the leader.
type Worker struct {
	d        Deps
	interval time.Duration
	mu       sync.Mutex
	status   map[string]*TargetStatus // key: bucket + "/" + target name
	ensured  map[string]bool          // destination buckets verified this process lifetime
	recent   map[string]time.Time     // target/version pushed synchronously; replay skips them
	kick     chan struct{}
}

// New creates a worker.
func New(d Deps, interval time.Duration) *Worker {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.ServerID == nil {
		d.ServerID = func() string { return "" }
	}
	if d.NewClient == nil {
		d.NewClient = func(t Target) Pusher { return newClient(t, d.ServerID()) }
	}
	if interval == 0 {
		interval = 10 * time.Second
	}
	return &Worker{d: d, interval: interval, status: map[string]*TargetStatus{}, ensured: map[string]bool{}, recent: map[string]time.Time{}, kick: make(chan struct{}, 1)}
}

// Trigger asks for a pass soon.
func (w *Worker) Trigger() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run loops until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
		if err := w.Pass(ctx); err != nil && ctx.Err() == nil {
			w.d.Log.Error("replication pass failed", "err", err)
		}
	}
}

// LoadRule reads a bucket's rule; ok=false when none.
func LoadRule(ctx context.Context, db *meta.DB, bucket string) (Rule, bool, error) {
	raw, err := db.GetSetting(ctx, RulePrefix+bucket)
	if err != nil || raw == "" {
		return Rule{}, false, err
	}
	var r Rule
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Rule{}, false, err
	}
	return r, true, nil
}

// Validate checks a bucket-level target, which needs a destination bucket.
func (t Target) Validate() error { return t.validate(true) }

// ValidateGlobal checks a server-wide target; the destination bucket is
// always the source bucket's name.
func (t Target) ValidateGlobal() error { return t.validate(false) }

func (t Target) validate(needBucket bool) error {
	if t.Name == "" {
		return errors.New("target name is required")
	}
	if len(t.Name) > maxTargetName || strings.ContainsAny(t.Name, " \t\r\n/") {
		return errors.New("target name must be at most 128 characters without spaces or slashes")
	}
	if !strings.HasPrefix(t.Endpoint, "http://") && !strings.HasPrefix(t.Endpoint, "https://") {
		return errors.New("endpoint must start with http:// or https://")
	}
	if t.AccessKey == "" || t.SecretKey == "" {
		return errors.New("access_key and secret_key are required")
	}
	if needBucket && t.Bucket == "" {
		return errors.New("destination bucket is required")
	}
	return nil
}

// Validate checks every bucket-level target and that names are unique.
func (r Rule) Validate() error { return r.validate(true) }

// ValidateGlobal checks every server-wide target and that names are unique.
func (r Rule) ValidateGlobal() error { return r.validate(false) }

func (r Rule) validate(needBucket bool) error {
	seen := map[string]bool{}
	for _, t := range r.Targets {
		if err := t.validate(needBucket); err != nil {
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate target name %q", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// LoadGlobal reads the server-wide rule; ok=false when none.
func LoadGlobal(ctx context.Context, db *meta.DB) (Rule, bool, error) {
	raw, err := db.GetSetting(ctx, GlobalKey)
	if err != nil || raw == "" {
		return Rule{}, false, err
	}
	var r Rule
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Rule{}, false, err
	}
	return r, true, nil
}

// merge adds the server-wide targets a bucket does not shadow with a target
// of the same name, each pointing at a bucket of the same name.
func merge(bucket string, own, global Rule) Rule {
	out := Rule{Targets: append([]Target(nil), own.Targets...)}
	for _, g := range global.Targets {
		if _, shadowed := own.Find(g.Name); shadowed {
			continue
		}
		g.Bucket = bucket
		g.Inherited = true
		out.Targets = append(out.Targets, g)
	}
	return out
}

// rules returns the effective rule of every bucket that has at least one
// target: its own targets plus the inherited server-wide ones.
func (w *Worker) rules(ctx context.Context) (map[string]Rule, error) {
	all, err := w.d.DB.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	own := map[string]Rule{}
	var global Rule
	for k, v := range all {
		switch {
		case k == GlobalKey:
			_ = json.Unmarshal([]byte(v), &global)
		case strings.HasPrefix(k, RulePrefix):
			var r Rule
			if json.Unmarshal([]byte(v), &r) == nil {
				own[strings.TrimPrefix(k, RulePrefix)] = r
			}
		}
	}
	out := map[string]Rule{}
	if len(global.Targets) > 0 {
		buckets, err := w.d.DB.ListBuckets(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, b := range buckets {
			out[b.Name] = merge(b.Name, own[b.Name], global)
		}
		// A deleted bucket's final entries (its deletion) still need pushing.
		logged, err := w.d.DB.ReplicationLogBuckets(ctx)
		if err != nil {
			return nil, err
		}
		for _, name := range logged {
			if name == ConfigBucket {
				continue
			}
			if _, done := out[name]; !done {
				out[name] = merge(name, own[name], global)
			}
		}
	}
	for name, r := range own {
		if _, done := out[name]; !done {
			out[name] = merge(name, r, global)
		}
	}
	for name, r := range out {
		if len(r.Targets) == 0 {
			delete(out, name)
		}
	}
	// Server-level entries (users, keys, settings) live under bucket "" and
	// go only to server-wide targets that opted into config sync.
	var sync []Target
	for _, g := range global.Targets {
		if g.SyncConfig {
			g.Inherited = true
			sync = append(sync, g)
		}
	}
	if len(sync) > 0 {
		out[ConfigBucket] = Rule{Targets: sync}
	}
	return out, nil
}

// ConfigBucket is the pseudo-bucket under which server-level changes are logged.
const ConfigBucket = ""

// effectiveRule is a bucket's own rule merged with the server-wide one.
func (w *Worker) effectiveRule(ctx context.Context, bucket string) (Rule, error) {
	global, _, err := LoadGlobal(ctx, w.d.DB)
	if err != nil {
		return Rule{}, err
	}
	if bucket == ConfigBucket {
		var sync []Target
		for _, g := range global.Targets {
			if g.SyncConfig {
				sync = append(sync, g)
			}
		}
		return Rule{Targets: sync}, nil
	}
	own, _, err := LoadRule(ctx, w.d.DB, bucket)
	if err != nil {
		return Rule{}, err
	}
	return merge(bucket, own, global), nil
}

const maxTargetName = 128

// CursorKey is the setting that holds a target's position in the log.
func CursorKey(bucket, target string) string { return CursorPrefix + bucket + ":" + target }

func (w *Worker) cursor(ctx context.Context, bucket, target string) int64 {
	v, _ := w.d.DB.GetSetting(ctx, CursorKey(bucket, target))
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (w *Worker) setCursor(ctx context.Context, bucket, target string, seq int64) error {
	res, err := w.d.Propose(ctx, &meta.SetSetting{Key: CursorKey(bucket, target), Value: strconv.FormatInt(seq, 10)})
	if err != nil {
		return err
	}
	return res.Error()
}

// Status returns the status of every bucket target, computing lag from the log.
func (w *Worker) Status(ctx context.Context) []TargetStatus {
	rules, err := w.rules(ctx)
	if err != nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []TargetStatus{}
	for bucket, rule := range rules {
		head, _ := w.d.DB.ReplicationHead(ctx, bucket)
		for _, t := range rule.Targets {
			st := w.status[bucket+"/"+t.Name]
			if st == nil {
				st = &TargetStatus{Bucket: bucket}
			}
			cp := *st
			cp.Bucket = bucket
			cp.Target = t
			cp.Target.SecretKey = "***"
			cp.Cursor = w.cursor(ctx, bucket, t.Name)
			cp.Head = head
			cp.Lag = head - cp.Cursor
			if cp.Lag < 0 {
				cp.Lag = 0
			}
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bucket != out[j].Bucket {
			return out[i].Bucket < out[j].Bucket
		}
		return out[i].Target.Name < out[j].Target.Name
	})
	return out
}

// Pass pushes pending entries for every enabled target, then trims the log.
func (w *Worker) Pass(ctx context.Context) error {
	rules, err := w.rules(ctx)
	if err != nil {
		return err
	}
	var errs []error
	live := map[string]bool{}
	for bucket, rule := range rules {
		for _, t := range rule.Targets {
			live[bucket+"/"+t.Name] = true
			if !t.Enabled {
				continue
			}
			if err := w.pushTarget(ctx, bucket, t); err != nil {
				errs = append(errs, fmt.Errorf("%s -> %s: %w", bucket, t.Name, err))
			}
		}
	}
	w.forgetExcept(live)
	if err := w.trim(ctx, rules); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// trim drops log entries every target has consumed. Sequence numbers are
// global, so a target's floor is its cursor while it still has pending
// entries for its bucket, and the log head otherwise: an idle or paused
// target with nothing pending must not pin entries of other buckets.
func (w *Worker) trim(ctx context.Context, rules map[string]Rule) error {
	count, _, head, err := w.d.DB.ReplicationLogStats(ctx)
	if err != nil || count == 0 {
		return err
	}
	floor := head
	for bucket, rule := range rules {
		for _, t := range rule.Targets {
			cur := w.cursor(ctx, bucket, t.Name)
			if cur >= floor {
				continue
			}
			pending, err := w.d.DB.ReplicationLog(ctx, bucket, cur, 1)
			if err != nil {
				return err
			}
			if len(pending) > 0 && pending[0].Seq-1 < floor {
				floor = pending[0].Seq - 1
			}
		}
	}
	if floor <= 0 {
		return nil
	}
	_, err = w.d.Propose(ctx, &meta.TrimReplicationLog{Seq: floor})
	return err
}

// forgetExcept drops in-memory status for targets that no longer exist.
func (w *Worker) forgetExcept(live map[string]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.status {
		if !live[k] {
			delete(w.status, k)
		}
	}
	for k := range w.ensured {
		if !live[k] {
			delete(w.ensured, k)
		}
	}
}

// stillConfigured reports whether the target is still part of the bucket's
// effective rule, so a push that raced a DELETE does not resurrect its cursor.
func (w *Worker) stillConfigured(ctx context.Context, bucket, target string) bool {
	rule, err := w.effectiveRule(ctx, bucket)
	if err != nil {
		return false
	}
	_, found := rule.Find(target)
	return found
}

// ensureBucket verifies (once per process and target) that the destination
// bucket exists, creating it when the destination says it does not.
func (w *Worker) ensureBucket(ctx context.Context, client Pusher, bucket string, t Target) error {
	key := bucket + "/" + t.Name
	w.mu.Lock()
	done := w.ensured[key]
	w.mu.Unlock()
	if done {
		return nil
	}
	opts := s3client.BucketOptions{}
	if b, err := w.d.DB.GetBucket(ctx, bucket); err == nil {
		opts.Versioned = b.Versioning == meta.VersioningEnabled
		opts.ObjectLock = b.ObjectLock
	}
	created, err := client.EnsureBucket(ctx, opts)
	if err != nil {
		return fmt.Errorf("destination bucket: %w", err)
	}
	if created {
		// A brand-new destination gets the source's current configuration;
		// later changes arrive as log entries.
		for _, kind := range []string{"policy", "cors", "lifecycle", "tagging", "lock", "notification"} {
			if err := w.pushBucketConfig(ctx, client, t, bucket, kind); err != nil {
				return fmt.Errorf("destination bucket %s: %w", kind, err)
			}
		}
	}
	w.mu.Lock()
	w.ensured[key] = true
	w.mu.Unlock()
	return nil
}

// pushBucketConfig writes one kind of bucket configuration to the target,
// taken from the bucket's current state.
func (w *Worker) pushBucketConfig(ctx context.Context, client Pusher, t Target, bucket, kind string) error {
	b, err := w.d.DB.GetBucket(ctx, bucket)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchBucket) {
			if kind == "delete" && t.Cellar {
				// Cellar members mirror bucket deletion, data included; other
				// targets keep their copy.
				w.mu.Lock()
				delete(w.ensured, bucket+"/"+t.Name)
				w.mu.Unlock()
				return client.PurgeBucket(ctx)
			}
			return nil // deleted meanwhile; nothing to mirror
		}
		return err
	}
	putOrDelete := func(sub, body string) error {
		if body == "" {
			return client.DeleteBucketConfig(ctx, sub)
		}
		return client.PutBucketConfig(ctx, sub, body)
	}
	switch kind {
	case "create":
		return nil // EnsureBucket already ran
	case "versioning":
		if b.Versioning == "" {
			return nil
		}
		return client.PutBucketConfig(ctx, "versioning", "<VersioningConfiguration><Status>"+b.Versioning+"</Status></VersioningConfiguration>")
	case "policy":
		return putOrDelete("policy", b.Policy)
	case "cors":
		return putOrDelete("cors", b.CORS)
	case "lifecycle":
		return putOrDelete("lifecycle", b.Lifecycle)
	case "tagging":
		v, _ := w.d.DB.GetSetting(ctx, "bucket.tagging:"+bucket)
		return putOrDelete("tagging", v)
	case "lock":
		if !b.ObjectLock {
			return nil
		}
		v, _ := w.d.DB.GetSetting(ctx, "bucket.lock:"+bucket)
		if v == "" {
			v = "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>"
		}
		return client.PutBucketConfig(ctx, "object-lock", v)
	case "notification":
		v, _ := w.d.DB.GetSetting(ctx, "bucket.notification:"+bucket)
		if v == "" {
			v = "<NotificationConfiguration></NotificationConfiguration>"
		}
		return client.PutBucketConfig(ctx, "notification", v)
	}
	return nil
}

// pushServer mirrors a user, access key or setting change through the
// target's admin API, replaying the current local state.
func (w *Worker) pushServer(ctx context.Context, client Pusher, e meta.ReplicationEntry) error {
	ignore404 := func(err error) error {
		var se *s3client.Error
		if errors.As(err, &se) && se.Status == 404 {
			return nil
		}
		return err
	}
	switch e.Op {
	case meta.ReplicationUser:
		u, err := w.d.DB.GetUserByName(ctx, e.Key)
		if errors.Is(err, meta.ErrNoSuchUser) {
			return ignore404(client.AdminJSON(ctx, "DELETE", "/users/"+url.PathEscape(e.Key), nil, nil))
		}
		if err != nil {
			return err
		}
		buckets := u.Buckets
		if buckets == nil {
			buckets = []string{}
		}
		return client.AdminJSON(ctx, "PUT", "/users/"+url.PathEscape(u.Name), map[string]any{
			"role": u.Role, "buckets": buckets, "disabled": u.Disabled, "password_hash": u.PasswordHash,
		}, nil)
	case meta.ReplicationKey:
		cred, err := w.d.DB.LookupAccessKey(ctx, e.Key)
		if errors.Is(err, meta.ErrNoSuchKeyID) {
			return ignore404(client.AdminJSON(ctx, "DELETE", "/keys/"+url.PathEscape(e.Key), nil, nil))
		}
		if err != nil {
			return err
		}
		return client.AdminJSON(ctx, "PUT", "/users/"+url.PathEscape(cred.User.Name)+"/keys/"+url.PathEscape(e.Key), map[string]any{
			"secret": cred.Key.Secret, "label": cred.Key.Label, "disabled": cred.Key.Disabled,
		}, nil)
	case meta.ReplicationSetting:
		v, err := w.d.DB.GetSetting(ctx, e.Key)
		if err != nil {
			return err
		}
		return client.AdminJSON(ctx, "PUT", "/settings/"+url.PathEscape(e.Key), map[string]string{"value": v}, nil)
	}
	return nil
}

func (w *Worker) record(bucket, target string, fn func(*TargetStatus)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := bucket + "/" + target
	st := w.status[key]
	if st == nil {
		st = &TargetStatus{Bucket: bucket}
		w.status[key] = st
	}
	fn(st)
}

func (w *Worker) pushTarget(ctx context.Context, bucket string, t Target) error {
	client := w.d.NewClient(t)
	cursor := w.cursor(ctx, bucket, t.Name)
	for {
		entries, err := w.d.DB.ReplicationLog(ctx, bucket, cursor, 200)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		if bucket != ConfigBucket {
			if err := w.ensureBucket(ctx, client, bucket, t); err != nil {
				w.record(bucket, t.Name, func(s *TargetStatus) { s.LastError = err.Error() })
				return err
			}
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if w.wanted(t, e) && !w.pushedRecently(t.Name, e.VersionID) {
				if err := w.push(ctx, client, t, e); err != nil {
					w.record(bucket, t.Name, func(s *TargetStatus) { s.LastError = err.Error() })
					return err
				}
				w.record(bucket, t.Name, func(s *TargetStatus) { s.Pushed++ })
			}
			cursor = e.Seq
		}
		if !w.stillConfigured(ctx, bucket, t.Name) {
			return nil
		}
		if err := w.setCursor(ctx, bucket, t.Name, cursor); err != nil {
			return err
		}
		w.record(bucket, t.Name, func(s *TargetStatus) { s.LastSync = w.d.Now(); s.LastError = "" })
		if len(entries) < 200 {
			return nil
		}
	}
}

// push replays one log entry against the destination.
// Object Lock metadata keys, as the API stores them.
const (
	lockModeKey  = "x-amz-object-lock-mode"
	lockUntilKey = "x-amz-object-lock-retain-until-date"
	legalHoldKey = "x-amz-object-lock-legal-hold"
	taggingKey   = "x-amz-tagging"
)

// objectHeaders builds the request headers that carry an object's metadata
// to another server: user metadata, content headers, tags and Object Lock
// state. A retention that already expired is left out, since a receiver
// refuses a retain-until date in the past and it protects nothing anyway.
func (w *Worker) objectHeaders(m map[string]string, contentType string) map[string]string {
	headers := map[string]string{}
	for k, val := range m {
		switch {
		case strings.HasPrefix(k, "x-amz-meta-"), k == "content-type", k == "content-encoding", k == "cache-control", k == "content-disposition", k == "content-language":
			headers[k] = val
		}
	}
	if contentType != "" {
		headers["content-type"] = contentType
	}
	if t := m[taggingKey]; t != "" {
		headers[taggingKey] = t
	}
	if mode, until := m[lockModeKey], m[lockUntilKey]; mode != "" && until != "" {
		if t, err := time.Parse(time.RFC3339, until); err == nil && t.After(w.d.Now()) {
			headers[lockModeKey], headers[lockUntilKey] = mode, until
		}
	}
	if h := m[legalHoldKey]; h != "" {
		headers[legalHoldKey] = h
	}
	return headers
}

// pushObjectMeta replays the current retention, legal hold and tags of one
// version. Retention is sent with governance bypass so a shortened
// governance period follows; compliance rules are enforced on both sides.
func (w *Worker) pushObjectMeta(ctx context.Context, client Pusher, e meta.ReplicationEntry) error {
	v, err := w.d.DB.GetObject(ctx, e.Bucket, e.Key, e.VersionID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchVersion) || errors.Is(err, meta.ErrNoSuchKey) {
			return nil
		}
		return err
	}
	if v.DeleteMarker {
		return nil
	}
	bypass := map[string]string{"x-amz-bypass-governance-retention": "true"}
	mode, until := v.Metadata[lockModeKey], v.Metadata[lockUntilKey]
	if mode != "" && until != "" {
		if t, err := time.Parse(time.RFC3339, until); err == nil && t.After(w.d.Now()) {
			body := "<Retention><Mode>" + mode + "</Mode><RetainUntilDate>" + until + "</RetainUntilDate></Retention>"
			if err := client.PutObjectSubresource(ctx, e.Key, e.VersionID, "retention", body, bypass); err != nil {
				return fmt.Errorf("retention: %w", err)
			}
		}
	} else if err := client.PutObjectSubresource(ctx, e.Key, e.VersionID, "retention", "<Retention></Retention>", bypass); err != nil {
		var se *s3client.Error
		// A destination without Object Lock has nothing to clear.
		if !errors.As(err, &se) || se.Status != 400 {
			return fmt.Errorf("retention: %w", err)
		}
	}
	hold := v.Metadata[legalHoldKey]
	if hold == "" {
		hold = "OFF"
	}
	if err := client.PutObjectSubresource(ctx, e.Key, e.VersionID, "legal-hold", "<LegalHold><Status>"+hold+"</Status></LegalHold>", nil); err != nil {
		var se *s3client.Error
		if hold != "OFF" || !errors.As(err, &se) || se.Status != 400 {
			return fmt.Errorf("legal hold: %w", err)
		}
	}
	if tags := v.Metadata[taggingKey]; tags != "" {
		values, err := url.ParseQuery(tags)
		if err != nil {
			return fmt.Errorf("tags: %w", err)
		}
		var b strings.Builder
		b.WriteString("<Tagging><TagSet>")
		for k, vs := range values {
			for _, val := range vs {
				b.WriteString("<Tag><Key>" + xmlEscape(k) + "</Key><Value>" + xmlEscape(val) + "</Value></Tag>")
			}
		}
		b.WriteString("</TagSet></Tagging>")
		if err := client.PutObjectSubresource(ctx, e.Key, e.VersionID, "tagging", b.String(), nil); err != nil {
			return fmt.Errorf("tags: %w", err)
		}
	} else if err := client.DeleteObjectSubresource(ctx, e.Key, e.VersionID, "tagging"); err != nil {
		return fmt.Errorf("tags: %w", err)
	}
	return nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// sourceHeaders tell a hogshead receiver which version id and time to reuse.
func sourceHeaders(vid string, at time.Time) map[string]string {
	h := map[string]string{}
	if vid != "" && vid != meta.NullVersionID {
		h[s3client.SourceVersionHeader] = vid
	}
	if !at.IsZero() {
		h[s3client.SourceTimeHeader] = strconv.FormatInt(at.UnixMilli(), 10)
	}
	return h
}

// wanted decides whether an entry goes to a target: replicated changes are
// never forwarded, a key prefix filters objects only, and server-level
// entries go to config-syncing targets only.
func (w *Worker) wanted(t Target, e meta.ReplicationEntry) bool {
	if e.Origin != "" {
		return false
	}
	switch e.Op {
	case meta.ReplicationPut, meta.ReplicationMarker, meta.ReplicationDelete, meta.ReplicationMeta:
		return t.Prefix == "" || strings.HasPrefix(e.Key, t.Prefix)
	case meta.ReplicationSetting:
		// Cellar membership state is for members only; a plain config-sync
		// target must never be told it belongs to a cellar.
		if strings.HasPrefix(e.Key, "cellar.") && !t.Cellar {
			return false
		}
		return t.SyncConfig
	case meta.ReplicationUser:
		if e.Key == CellarServiceUser && !t.Cellar {
			return false
		}
		return t.SyncConfig
	case meta.ReplicationKey:
		if e.VersionID == CellarServiceUser && !t.Cellar {
			return false
		}
		return t.SyncConfig
	}
	return true
}

// CellarServiceUser is the administrator account cellar members share; it
// and its keys are mirrored to cellar members only.
const CellarServiceUser = "cellar"

func (w *Worker) push(ctx context.Context, client Pusher, t Target, e meta.ReplicationEntry) error {
	switch e.Op {
	case meta.ReplicationBucket:
		return w.pushBucketConfig(ctx, client, t, e.Bucket, e.Key)
	case meta.ReplicationUser, meta.ReplicationKey, meta.ReplicationSetting:
		return w.pushServer(ctx, client, e)
	case meta.ReplicationMarker:
		// The marker keeps its id and time on the other side, so a second
		// delivery is a no-op and a delayed marker never hides a newer write.
		return client.DeleteObjectVersion(ctx, e.Key, "", sourceHeaders(e.VersionID, e.At))
	case meta.ReplicationDelete:
		if e.VersionID != meta.NullVersionID {
			// Version ids are shared between hogshead servers, so purging a
			// version maps onto the same version there. Other services, or
			// copies made before ids were shared, return 404, which is ignored.
			return client.DeleteObjectVersion(ctx, e.Key, e.VersionID, nil)
		}
		if _, err := w.d.DB.GetObject(ctx, e.Bucket, e.Key, ""); err == nil {
			return nil // overwritten, the following put carries the new content
		}
		return client.DeleteObject(ctx, e.Key)
	case meta.ReplicationPut:
		v, err := w.d.DB.GetObject(ctx, e.Bucket, e.Key, e.VersionID)
		if err != nil {
			if errors.Is(err, meta.ErrNoSuchVersion) || errors.Is(err, meta.ErrNoSuchKey) {
				return nil // already gone; a later entry covers it
			}
			return err
		}
		if v.DeleteMarker {
			return client.DeleteObject(ctx, e.Key)
		}
		rc, err := store.Open(ctx, w.d.Chunks, v.Chunks, 0, -1)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		headers := w.objectHeaders(v.Metadata, v.ContentType)
		for k, val := range sourceHeaders(v.VersionID, v.Created) {
			headers[k] = val
		}
		return client.PutObject(ctx, e.Key, rc, v.Size, headers)
	case meta.ReplicationMeta:
		return w.pushObjectMeta(ctx, client, e)
	}
	return nil
}

// ---- synchronous push for cellar write acknowledgement ----

// Write acknowledgement levels.
const (
	AckLocal = "local"
	AckOne   = "one"
	AckAll   = "all"
)

// ErrNotAcknowledged is returned when too few cellar members confirmed a write.
var ErrNotAcknowledged = errors.New("write not acknowledged by enough cellar members")

const recentTTL = 10 * time.Minute

func (w *Worker) markPushed(target, vid string) {
	if vid == "" || vid == meta.NullVersionID {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.d.Now()
	if len(w.recent) > 50000 {
		for k, t := range w.recent {
			if now.Sub(t) > recentTTL {
				delete(w.recent, k)
			}
		}
	}
	w.recent[target+"/"+vid] = now
}

func (w *Worker) pushedRecently(target, vid string) bool {
	if vid == "" || vid == meta.NullVersionID {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.recent[target+"/"+vid]
	if ok && w.d.Now().Sub(t) > recentTTL {
		delete(w.recent, target+"/"+vid)
		return false
	}
	return ok
}

// cellarTargets returns the enabled cellar targets for a bucket.
func (w *Worker) cellarTargets(ctx context.Context, bucket string) []Target {
	global, _, err := LoadGlobal(ctx, w.d.DB)
	if err != nil {
		return nil
	}
	var out []Target
	for _, t := range global.Targets {
		if t.Cellar && t.Enabled {
			t.Bucket = bucket
			t.Inherited = true
			out = append(out, t)
		}
	}
	return out
}

// need translates the acknowledgement level into a number of confirmations.
func (w *Worker) need(ctx context.Context, peers int) int {
	if peers == 0 || w.d.WriteAck == nil {
		return 0
	}
	switch w.d.WriteAck(ctx) {
	case AckOne:
		return 1
	case AckAll:
		return peers
	}
	return 0
}

// pushFn pushes one change to one target.
type pushFn func(ctx context.Context, client Pusher, t Target) error

// syncPush runs fn against every cellar target concurrently and returns once
// `need` succeeded, or ErrNotAcknowledged when that is no longer possible.
// Successful pushes are remembered so the log replay does not repeat them.
func (w *Worker) syncPush(ctx context.Context, bucket, vid string, fn pushFn) error {
	targets := w.cellarTargets(ctx, bucket)
	need := w.need(ctx, len(targets))
	if need == 0 {
		return nil
	}
	// The pushes run detached from the request: once enough members have
	// confirmed, the caller returns while the slower ones finish on their
	// own. Cancelling them would abort an upload midway, which the receiver
	// logs as a failed request before the replay delivers it again.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	var pending sync.WaitGroup
	pending.Add(len(targets))
	go func() { pending.Wait(); cancel() }() //nolint:gosec // G118: bounded by the timeout above
	results := make(chan error, len(targets))
	for _, t := range targets {
		go func(t Target) {
			defer pending.Done()
			client := w.d.NewClient(t)
			if err := w.ensureBucket(ctx, client, bucket, t); err != nil {
				results <- err
				return
			}
			err := fn(ctx, client, t)
			if err == nil {
				w.markPushed(t.Name, vid)
			}
			results <- err
		}(t)
	}
	ok, failed := 0, 0
	var last error
	for range targets {
		if err := <-results; err != nil {
			failed++
			last = err
		} else {
			ok++
		}
		if ok >= need {
			return nil
		}
		if failed > len(targets)-need {
			break
		}
	}
	if last != nil {
		return fmt.Errorf("%w: %w", ErrNotAcknowledged, last)
	}
	return ErrNotAcknowledged
}

// SyncPut pushes a new object version to cellar members before the local
// commit. cmd must carry the chunks, metadata, version id and time that
// will be committed.
func (w *Worker) SyncPut(ctx context.Context, cmd *meta.PutObjectVersion) error {
	return w.syncPush(ctx, cmd.Bucket, cmd.VersionID, func(ctx context.Context, client Pusher, _ Target) error {
		rc, err := store.Open(ctx, w.d.Chunks, cmd.Chunks, 0, -1)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		headers := w.objectHeaders(cmd.Metadata, cmd.ContentType)
		for k, val := range sourceHeaders(cmd.VersionID, time.UnixMilli(cmd.Now)) {
			headers[k] = val
		}
		return client.PutObject(ctx, cmd.Key, rc, cmd.Size, headers)
	})
}

// SyncDelete pushes a delete to cellar members before the local commit:
// a marker (with its id and time) on versioned buckets, a versioned purge
// when cmd names a version, or a plain delete otherwise.
func (w *Worker) SyncDelete(ctx context.Context, cmd *meta.DeleteObject, versioned bool) error {
	vid := cmd.VersionID
	if vid == "" && versioned {
		vid = cmd.MarkerVersionID
	}
	return w.syncPush(ctx, cmd.Bucket, vid, func(ctx context.Context, client Pusher, _ Target) error {
		switch {
		case cmd.VersionID != "":
			return client.DeleteObjectVersion(ctx, cmd.Key, cmd.VersionID, nil)
		case versioned:
			return client.DeleteObjectVersion(ctx, cmd.Key, "", sourceHeaders(cmd.MarkerVersionID, time.UnixMilli(cmd.Now)))
		default:
			return client.DeleteObject(ctx, cmd.Key)
		}
	})
}

// SkipBacklog moves a target's cursors to the current head of every bucket
// log so that nothing logged before now is replayed to it. A new cellar
// member receives a backfill of current state instead, which keeps a merge
// deterministic: the joiner's old "create user" entries, for example, must
// not overwrite the cellar's definition after the backfill applied it.
func (w *Worker) SkipBacklog(ctx context.Context, target string) error {
	buckets, err := w.d.DB.ReplicationLogBuckets(ctx)
	if err != nil {
		return err
	}
	buckets = append(buckets, ConfigBucket)
	seen := map[string]bool{}
	for _, b := range buckets {
		if seen[b] {
			continue
		}
		seen[b] = true
		head, err := w.d.DB.ReplicationHead(ctx, b)
		if err != nil {
			return err
		}
		if head == 0 {
			continue
		}
		if err := w.setCursor(ctx, b, target, head); err != nil {
			return err
		}
	}
	return nil
}

// targetHasVersion asks the target for one version. A delete marker answers
// HEAD with 405 (and x-amz-delete-marker), which also means "present".
func (w *Worker) targetHasVersion(ctx context.Context, client Pusher, key, vid string) bool {
	if vid == "" || vid == meta.NullVersionID {
		return false
	}
	_, err := client.HeadObjectVersion(ctx, key, vid)
	if err == nil {
		return true
	}
	var se *s3client.Error
	return errors.As(err, &se) && se.Status == 405
}

// Backfill brings a target up to date from current state rather than from
// the log: every bucket with every version and delete marker it holds
// (skipping those the target already has), then users and keys when the
// target syncs configuration. It is what a new cellar member receives, and
// it is safe to run repeatedly.
//
// configWins decides clashes when both sides already have something: when
// true this server's bucket configuration, user definitions and settings
// overwrite the target's; when false they are pushed only for buckets and
// users the target does not have yet. A cellar passes true from the
// longer-standing member, so a merge is deterministic.
func (w *Worker) Backfill(ctx context.Context, t Target, configWins bool, progress func(done, total int)) error {
	buckets, err := w.d.DB.ListBuckets(ctx, "")
	if err != nil {
		return err
	}
	total := 0
	for _, b := range buckets {
		if v, _, err := w.d.DB.BucketVersionStats(ctx, b.Name); err == nil {
			total += int(v)
		}
	}
	done := 0
	for _, b := range buckets {
		bt := t
		bt.Bucket = b.Name
		client := w.d.NewClient(bt)
		opts := s3client.BucketOptions{Versioned: b.Versioning == meta.VersioningEnabled, ObjectLock: b.ObjectLock}
		created, err := client.EnsureBucket(ctx, opts)
		if err != nil {
			return fmt.Errorf("%s: %w", b.Name, err)
		}
		w.mu.Lock()
		w.ensured[b.Name+"/"+t.Name] = true
		w.mu.Unlock()
		if created || configWins {
			for _, kind := range []string{"versioning", "policy", "cors", "lifecycle", "tagging", "lock", "notification"} {
				if err := w.pushBucketConfig(ctx, client, bt, b.Name, kind); err != nil {
					return fmt.Errorf("%s %s: %w", b.Name, kind, err)
				}
			}
		}
		vopts := meta.VersionListOpts{Max: 1000}
		for {
			res, err := w.d.DB.ListVersions(ctx, b.Name, vopts)
			if err != nil {
				return err
			}
			// Oldest first within a key, so arrival order matches history.
			page := append([]meta.ObjectVersion(nil), res.Versions...)
			sort.SliceStable(page, func(i, j int) bool {
				if page[i].Key != page[j].Key {
					return page[i].Key < page[j].Key
				}
				return page[i].Created.Before(page[j].Created)
			})
			for i := range page {
				v := page[i]
				if err := ctx.Err(); err != nil {
					return err
				}
				if w.targetHasVersion(ctx, client, v.Key, v.VersionID) {
					done++
					continue
				}
				op := meta.ReplicationPut
				if v.DeleteMarker {
					op = meta.ReplicationMarker
				}
				e := meta.ReplicationEntry{Bucket: b.Name, Key: v.Key, VersionID: v.VersionID, Op: op, At: v.Created}
				if err := w.push(ctx, client, bt, e); err != nil {
					return fmt.Errorf("%s/%s: %w", b.Name, v.Key, err)
				}
				w.markPushed(t.Name, v.VersionID)
				done++
				if progress != nil {
					progress(done, total)
				}
			}
			if !res.IsTruncated {
				break
			}
			vopts.KeyMarker, vopts.VersionIDMarker = res.NextKeyMarker, res.NextVersionIDMarker
		}
	}
	if !t.SyncConfig {
		return nil
	}
	client := w.d.NewClient(t)
	users, err := w.d.DB.ListUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Name == CellarServiceUser && !t.Cellar {
			continue
		}
		if !configWins {
			// Only users the target lacks; existing definitions there win.
			err := client.AdminJSON(ctx, "GET", "/users/"+url.PathEscape(u.Name), nil, nil)
			var se *s3client.Error
			switch {
			case err == nil:
				goto keys
			case errors.As(err, &se) && se.Status == 404:
			default:
				return fmt.Errorf("user %s: %w", u.Name, err)
			}
		}
		if err := w.pushServer(ctx, client, meta.ReplicationEntry{Op: meta.ReplicationUser, Key: u.Name}); err != nil {
			return fmt.Errorf("user %s: %w", u.Name, err)
		}
	keys:
		keys, err := w.d.DB.ListAccessKeys(ctx, u.ID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := w.pushServer(ctx, client, meta.ReplicationEntry{Op: meta.ReplicationKey, Key: k.AccessKeyID, VersionID: u.Name}); err != nil {
				return fmt.Errorf("key %s: %w", k.AccessKeyID, err)
			}
		}
	}
	if !configWins {
		return nil // the target's settings win
	}
	all, err := w.d.DB.AllSettings(ctx)
	if err != nil {
		return err
	}
	for k := range all {
		if !meta.ReplicableSetting(k) || (strings.HasPrefix(k, "cellar.") && !t.Cellar) {
			continue
		}
		if err := w.pushServer(ctx, client, meta.ReplicationEntry{Op: meta.ReplicationSetting, Key: k}); err != nil {
			return fmt.Errorf("setting %s: %w", k, err)
		}
	}
	return nil
}
