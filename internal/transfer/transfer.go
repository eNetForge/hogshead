// Package transfer runs one-shot copy jobs between a local bucket and a
// bucket on another S3 endpoint: export (local to remote) to migrate away or
// seed a standby, import (remote to local) to migrate in. Jobs run inside the
// server, report progress, and leave a record in the metadata store.
package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/s3client"
	"github.com/enetforge/hogshead/internal/store"
)

// Direction of a job.
type Direction string

const (
	// Export copies from the local bucket to the remote one.
	Export Direction = "export"
	// Import copies from the remote bucket into the local one.
	Import Direction = "import"
)

// Overwrite policies for keys that already exist at the destination.
const (
	OverwriteIfChanged = "if-changed" // copy unless size and ETag match (default)
	OverwriteAlways    = "always"
	OverwriteNever     = "never"
)

// Job states.
const (
	StateQueued      = "queued"
	StateRunning     = "running"
	StateDone        = "done"
	StateFailed      = "failed"
	StateCancelled   = "cancelled"
	StateInterrupted = "interrupted" // the server stopped while it ran
)

// Spec describes a job.
type Spec struct {
	Direction Direction `json:"direction"`
	// Bucket and Prefix select the local side. Keys are mapped by replacing
	// Prefix with RemotePrefix (and the reverse for imports).
	Bucket       string            `json:"bucket"`
	Prefix       string            `json:"prefix,omitempty"`
	Remote       s3client.Endpoint `json:"remote"`
	RemotePrefix string            `json:"remote_prefix,omitempty"`
	Overwrite    string            `json:"overwrite,omitempty"`
	DryRun       bool              `json:"dry_run,omitempty"`
	Concurrency  int               `json:"concurrency,omitempty"`
	// OwnerID owns objects created by an import.
	OwnerID string `json:"owner_id,omitempty"`
}

// Validate checks a spec and fills defaults.
func (s *Spec) Validate() error {
	switch s.Direction {
	case Export, Import:
	default:
		return errors.New("direction must be import or export")
	}
	if s.Bucket == "" {
		return errors.New("bucket is required")
	}
	if err := s.Remote.Validate(true); err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	switch s.Overwrite {
	case "":
		s.Overwrite = OverwriteIfChanged
	case OverwriteIfChanged, OverwriteAlways, OverwriteNever:
	default:
		return errors.New("overwrite must be if-changed, always or never")
	}
	if s.Concurrency <= 0 {
		s.Concurrency = 4
	}
	if s.Concurrency > 16 {
		s.Concurrency = 16
	}
	return nil
}

// Job is a spec plus its progress. Secrets are never included.
type Job struct {
	ID         string    `json:"id"`
	Actor      string    `json:"actor"`
	Spec       Spec      `json:"spec"`
	State      string    `json:"state"`
	Listed     int64     `json:"listed"`
	Copied     int64     `json:"copied"`
	Skipped    int64     `json:"skipped"`
	Failed     int64     `json:"failed"`
	Bytes      int64     `json:"bytes"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
	Errors     []string  `json:"errors,omitempty"` // first failures, "key: reason"
}

func (j *Job) terminal() bool {
	switch j.State {
	case StateDone, StateFailed, StateCancelled, StateInterrupted:
		return true
	}
	return false
}

// Client is the remote side; satisfied by *s3client.Client.
type Client interface {
	EnsureBucket(ctx context.Context, o s3client.BucketOptions) (bool, error)
	HeadObject(ctx context.Context, key string) (*s3client.ObjectInfo, error)
	GetObject(ctx context.Context, key string) (*s3client.Object, error)
	PutObject(ctx context.Context, key string, body io.Reader, size int64, headers map[string]string) error
	ListObjects(ctx context.Context, prefix, token string) (*s3client.Listing, error)
}

// Deps are the manager's collaborators.
type Deps struct {
	DB        *meta.DB
	Propose   func(ctx context.Context, cmd meta.Command) (meta.Result, error)
	Chunks    store.ChunkStore
	Cfg       func() config.Config
	Log       *slog.Logger
	Now       func() time.Time
	NewClient func(e s3client.Endpoint) Client
}

// Manager starts and tracks jobs.
type Manager struct {
	d       Deps
	ctx     context.Context
	mu      sync.Mutex
	running map[string]*running
}

type running struct {
	mu     sync.Mutex
	job    Job
	spec   Spec // with the secret
	cancel context.CancelFunc
}

const maxErrors = 20

// New builds a manager; jobs run until ctx ends.
func New(ctx context.Context, d Deps) *Manager {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.NewClient == nil {
		d.NewClient = func(e s3client.Endpoint) Client { return s3client.New(e) }
	}
	return &Manager{d: d, ctx: ctx, running: map[string]*running{}}
}

// MarkInterrupted flags jobs the previous process left running.
func (m *Manager) MarkInterrupted(ctx context.Context) error {
	rows, err := m.d.DB.ListTransfers(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		var j Job
		if json.Unmarshal([]byte(r.Data), &j) != nil || j.terminal() {
			continue
		}
		j.State = StateInterrupted
		j.FinishedAt = m.d.Now()
		j.Error = "the server stopped while the job was running"
		if err := m.persist(ctx, &j); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) persist(ctx context.Context, j *Job) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	res, err := m.d.Propose(ctx, &meta.PutTransfer{ID: j.ID, Created: j.CreatedAt.UnixMilli(), Data: string(raw)})
	if err != nil {
		return err
	}
	return res.Error()
}

// Start validates the spec, checks the local bucket and launches the job.
func (m *Manager) Start(ctx context.Context, spec Spec, actor string) (*Job, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	b, err := m.d.DB.GetBucket(ctx, spec.Bucket)
	if err != nil {
		return nil, err
	}
	if spec.OwnerID == "" {
		spec.OwnerID = b.Owner
	}
	now := m.d.Now()
	public := spec
	public.Remote.SecretKey = ""
	job := Job{ID: ids.TransferID(), Actor: actor, Spec: public, State: StateQueued, CreatedAt: now}
	if err := m.persist(ctx, &job); err != nil {
		return nil, err
	}
	// Jobs outlive the HTTP request that started them, so they run under the
	// manager's lifetime context rather than the request's.
	jctx, cancel := context.WithCancel(m.ctx)
	r := &running{job: job, spec: spec, cancel: cancel}
	m.mu.Lock()
	m.running[job.ID] = r
	m.mu.Unlock()
	go m.run(jctx, r) //nolint:gosec // G118: intentional, see above
	return &job, nil
}

// List returns every job, running ones with live counters, newest first.
func (m *Manager) List(ctx context.Context) ([]Job, error) {
	rows, err := m.d.DB.ListTransfers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(rows))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range rows {
		if r, ok := m.running[row.ID]; ok {
			out = append(out, r.snapshot())
			continue
		}
		var j Job
		if json.Unmarshal([]byte(row.Data), &j) == nil {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].CreatedAt.After(out[k].CreatedAt) })
	return out, nil
}

// Get returns one job.
func (m *Manager) Get(ctx context.Context, id string) (*Job, error) {
	m.mu.Lock()
	r, ok := m.running[id]
	m.mu.Unlock()
	if ok {
		j := r.snapshot()
		return &j, nil
	}
	row, err := m.d.DB.GetTransfer(ctx, id)
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal([]byte(row.Data), &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// Cancel stops a running job; it reports ErrNotRunning otherwise.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	r, ok := m.running[id]
	m.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	r.cancel()
	return nil
}

// ErrNotRunning is returned by Cancel for a job that is not running.
var ErrNotRunning = errors.New("job is not running")

// Delete removes the record of a finished job.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	_, ok := m.running[id]
	m.mu.Unlock()
	if ok {
		return errors.New("job is still running; cancel it first")
	}
	res, err := m.d.Propose(ctx, &meta.DeleteTransfer{ID: id})
	if err != nil {
		return err
	}
	return res.Error()
}

func (r *running) snapshot() Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.job
	j.Errors = append([]string(nil), r.job.Errors...)
	return j
}

func (r *running) update(fn func(j *Job)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.job)
}

// fail records a per-object failure, except when the job itself was cancelled.
func (r *running) fail(ctx context.Context, key string, err error) {
	if ctx.Err() != nil {
		return
	}
	r.update(func(j *Job) {
		j.Failed++
		if len(j.Errors) < maxErrors {
			j.Errors = append(j.Errors, key+": "+err.Error())
		}
	})
}

// run executes a job to completion.
func (m *Manager) run(ctx context.Context, r *running) {
	defer r.cancel()
	r.update(func(j *Job) { j.State = StateRunning; j.StartedAt = m.d.Now() })
	_ = m.persist(m.ctx, ptr(r.snapshot()))

	stop := make(chan struct{})
	go func() { // periodic progress persistence
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = m.persist(m.ctx, ptr(r.snapshot()))
			}
		}
	}()

	err := m.execute(ctx, r)
	close(stop)
	r.update(func(j *Job) {
		j.FinishedAt = m.d.Now()
		switch {
		case err != nil && errors.Is(err, context.Canceled) && m.ctx.Err() == nil:
			j.State = StateCancelled
		case err != nil && m.ctx.Err() != nil:
			j.State = StateInterrupted
			j.Error = "the server stopped while the job was running"
		case err != nil:
			j.State = StateFailed
			j.Error = err.Error()
		case j.Failed > 0:
			j.State = StateFailed
			j.Error = fmt.Sprintf("%d object(s) failed", j.Failed)
		default:
			j.State = StateDone
		}
	})
	final := r.snapshot()
	m.mu.Lock()
	delete(m.running, final.ID)
	m.mu.Unlock()
	pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.persist(pctx, &final); err != nil {
		m.d.Log.Error("transfer: persist final state", "id", final.ID, "err", err)
	}
	m.d.Log.Info("transfer finished", "id", final.ID, "state", final.State, "copied", final.Copied, "skipped", final.Skipped, "failed", final.Failed, "bytes", final.Bytes)
}

func ptr[T any](v T) *T { return &v }

type item struct {
	local  *meta.ObjectVersion // export
	remote *s3client.ObjectInfo
}

func (m *Manager) execute(ctx context.Context, r *running) error {
	spec := r.spec
	client := m.d.NewClient(spec.Remote)
	if spec.Direction == Export && !spec.DryRun {
		opts := s3client.BucketOptions{}
		if b, err := m.d.DB.GetBucket(ctx, spec.Bucket); err == nil {
			opts.Versioned = b.Versioning == meta.VersioningEnabled
			opts.ObjectLock = b.ObjectLock
		}
		if _, err := client.EnsureBucket(ctx, opts); err != nil {
			return fmt.Errorf("remote bucket: %w", err)
		}
	}
	items := make(chan item, 64)
	var wg sync.WaitGroup
	for i := 0; i < spec.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range items {
				if ctx.Err() != nil {
					continue
				}
				if spec.Direction == Export {
					m.exportOne(ctx, r, client, it.local)
				} else {
					m.importOne(ctx, r, client, it.remote)
				}
			}
		}()
	}
	var listErr error
	if spec.Direction == Export {
		listErr = m.listLocal(ctx, r, items)
	} else {
		listErr = m.listRemote(ctx, r, client, items)
	}
	close(items)
	wg.Wait()
	if listErr != nil {
		return listErr
	}
	return ctx.Err()
}

func (m *Manager) listLocal(ctx context.Context, r *running, items chan<- item) error {
	cursor := ""
	for {
		res, err := m.d.DB.ListObjects(ctx, r.spec.Bucket, meta.ListOpts{Prefix: r.spec.Prefix, StartAfter: cursor, Max: 1000})
		if err != nil {
			return err
		}
		for i := range res.Objects {
			v := res.Objects[i]
			r.update(func(j *Job) { j.Listed++ })
			select {
			case items <- item{local: &v}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if !res.IsTruncated {
			return nil
		}
		cursor = res.NextCursor
	}
}

func (m *Manager) listRemote(ctx context.Context, r *running, client Client, items chan<- item) error {
	token := ""
	for {
		page, err := client.ListObjects(ctx, r.spec.RemotePrefix, token)
		if err != nil {
			return fmt.Errorf("list remote: %w", err)
		}
		for i := range page.Objects {
			o := page.Objects[i]
			if strings.HasSuffix(o.Key, "/") && o.Size == 0 {
				continue // folder placeholders
			}
			r.update(func(j *Job) { j.Listed++ })
			select {
			case items <- item{remote: &o}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if !page.Truncated || page.NextToken == "" {
			return nil
		}
		token = page.NextToken
	}
}

// unchanged decides whether a destination copy can be kept: equal ETags, or
// equal sizes when either ETag is a multipart composite that cannot match.
func unchanged(srcETag string, srcSize int64, dstETag string, dstSize int64) bool {
	if srcSize != dstSize {
		return false
	}
	if srcETag == dstETag {
		return true
	}
	return strings.Contains(srcETag, "-") || strings.Contains(dstETag, "-")
}

// copyHeaders picks the metadata that travels with an object.
func copyHeaders(src map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range src {
		k = strings.ToLower(k)
		switch {
		case strings.HasPrefix(k, "x-amz-meta-"), k == "content-type", k == "content-encoding", k == "cache-control",
			k == "content-disposition", k == "content-language", k == "expires", k == "x-amz-tagging", k == "x-amz-storage-class":
			out[k] = v
		}
	}
	return out
}

func (m *Manager) exportOne(ctx context.Context, r *running, client Client, v *meta.ObjectVersion) {
	remoteKey := r.spec.RemotePrefix + strings.TrimPrefix(v.Key, r.spec.Prefix)
	if r.spec.Overwrite != OverwriteAlways {
		cur, err := client.HeadObject(ctx, remoteKey)
		switch {
		case err == nil && (r.spec.Overwrite == OverwriteNever || unchanged(v.ETag, v.Size, cur.ETag, cur.Size)):
			r.update(func(j *Job) { j.Skipped++ })
			return
		case err != nil && !errors.Is(err, s3client.ErrNotFound):
			r.fail(ctx, v.Key, err)
			return
		}
	}
	if r.spec.DryRun {
		r.update(func(j *Job) { j.Copied++; j.Bytes += v.Size })
		return
	}
	rc, err := store.Open(ctx, m.d.Chunks, v.Chunks, 0, -1)
	if err != nil {
		r.fail(ctx, v.Key, err)
		return
	}
	defer func() { _ = rc.Close() }()
	headers := copyHeaders(v.Metadata)
	if v.ContentType != "" {
		headers["content-type"] = v.ContentType
	}
	if v.StorageClass != "" && v.StorageClass != "STANDARD" {
		headers["x-amz-storage-class"] = v.StorageClass
	}
	if err := client.PutObject(ctx, remoteKey, rc, v.Size, headers); err != nil {
		r.fail(ctx, v.Key, err)
		return
	}
	r.update(func(j *Job) { j.Copied++; j.Bytes += v.Size })
}

func (m *Manager) importOne(ctx context.Context, r *running, client Client, o *s3client.ObjectInfo) {
	localKey := r.spec.Prefix + strings.TrimPrefix(o.Key, r.spec.RemotePrefix)
	if r.spec.Overwrite != OverwriteAlways {
		cur, err := m.d.DB.GetObject(ctx, r.spec.Bucket, localKey, "")
		if err == nil && !cur.DeleteMarker && (r.spec.Overwrite == OverwriteNever || unchanged(o.ETag, o.Size, cur.ETag, cur.Size)) {
			r.update(func(j *Job) { j.Skipped++ })
			return
		}
	}
	if r.spec.DryRun {
		r.update(func(j *Job) { j.Copied++; j.Bytes += o.Size })
		return
	}
	obj, err := client.GetObject(ctx, o.Key)
	if err != nil {
		r.fail(ctx, o.Key, err)
		return
	}
	defer func() { _ = obj.Body.Close() }()
	cfg := m.d.Cfg()
	res, err := store.Write(ctx, m.d.Chunks, obj.Body, store.WriteOpts{ChunkSize: cfg.Storage.ChunkSize, MaxSize: cfg.Limits.MaxObjectSize})
	if err != nil {
		r.fail(ctx, o.Key, err)
		return
	}
	headers := copyHeaders(obj.Headers)
	ct := headers["content-type"]
	sc := headers["x-amz-storage-class"]
	delete(headers, "x-amz-storage-class")
	cmd := &meta.PutObjectVersion{
		Bucket: r.spec.Bucket, Key: localKey, VersionID: ids.VersionID(),
		Size: res.Size, ETag: res.MD5, ContentType: ct, Metadata: headers,
		Chunks: res.Chunks, Checksums: res.Checksums, StorageClass: sc,
		Owner: r.spec.OwnerID, Now: m.d.Now().UnixMilli(),
	}
	pres, err := m.d.Propose(ctx, cmd)
	if err == nil {
		err = pres.Error()
	}
	if err != nil {
		r.fail(ctx, o.Key, err)
		return
	}
	r.update(func(j *Job) { j.Copied++; j.Bytes += res.Size })
}
