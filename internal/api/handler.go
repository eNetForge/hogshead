package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enetforge/hogshead/internal/api/s3err"
	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/notify"
	"github.com/enetforge/hogshead/internal/policy"
	"github.com/enetforge/hogshead/internal/s3client"
	"github.com/enetforge/hogshead/internal/store"
	"github.com/enetforge/hogshead/internal/usage"
)

// Deps are the collaborators the S3 handler needs.
type Deps struct {
	DB      *meta.DB
	Propose func(ctx context.Context, cmd meta.Command) (meta.Result, error)
	Chunks  store.ChunkStore
	Cfg     func() config.Config
	Log     *slog.Logger
	Now     func() time.Time
	Region  string
	// Writable returns an error when the server must refuse writes.
	Writable func() error
	// Session authenticates web UI requests by cookie; nil disables it.
	Session SessionAuth
	// Sync, when set, pushes writes to cellar members before they are
	// committed and fails the request when too few acknowledge.
	Sync SyncReplicator
	// OnChange is called after a committed object change so replication can
	// run immediately instead of waiting for its next pass.
	OnChange func()
	// Access receives one record per finished request (access log, usage).
	Access func(usage.Record)
	// Notify delivers bucket event notifications; nil disables them.
	Notify *notify.Dispatcher
	// Optional metrics hooks.
	Observe     func(op string, status int, dur time.Duration)
	BytesIn     func(float64)
	BytesOut    func(float64)
	AuthFailure func()
}

// Handler serves the S3 API.
type Handler struct {
	d        Deps
	verifier *auth.Verifier
	limiter  *authLimiter
	ops      map[Operation]opFunc
}

// New builds the S3 handler.
func New(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Region == "" {
		d.Region = "us-east-1"
	}
	if d.Writable == nil {
		d.Writable = func() error { return nil }
	}
	h := &Handler{d: d, limiter: newAuthLimiter()}
	h.ops = h.buildDispatch()
	h.verifier = &auth.Verifier{
		Lookup: func(ctx context.Context, ak string) (string, error) {
			c, err := d.DB.LookupAccessKey(ctx, ak)
			if err != nil {
				if errors.Is(err, meta.ErrNoSuchKeyID) {
					return "", auth.ErrUnknownKey
				}
				return "", err
			}
			return c.Key.Secret, nil
		},
		Now:                 d.Now,
		Skew:                d.Cfg().Limits.ClockSkewWindow.Std(),
		VirtualHostSuffixes: hostnames(d.Cfg()),
		AllowV2:             true,
	}
	return h
}

// reqCtx carries one request through the pipeline.
type reqCtx struct {
	errCode, errMsg string // the S3 error written, if any
	w               http.ResponseWriter
	r               *http.Request
	id              string
	target          Target
	op              Operation
	caller          *policy.Caller
	cred            *meta.Credential
	ident           *auth.Identity
	user            *meta.User // the authenticated user, from an access key or a session
	bucket          *meta.Bucket
	pol             *policy.Policy
	started         time.Time
	polBroken       bool
}

func (c *reqCtx) ctx() context.Context { return c.r.Context() }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := &reqCtx{w: &statusWriter{ResponseWriter: w}, r: r, id: ids.RequestID(), started: h.d.Now()}
	hdr := w.Header()
	hdr.Set("x-amz-request-id", c.id)
	hdr.Set("x-amz-id-2", c.id)
	hdr.Set("Server", "hogshead")
	hdr.Set("Accept-Ranges", "bytes")

	cfg := h.d.Cfg()
	c.target = ResolveTarget(r, hostnames(cfg))
	c.op = Route(r.Method, c.target, WithHeaders(r.URL.Query(), r.Header))

	err := h.serve(c)
	if err != nil {
		h.writeError(c, err)
	}
	sw := c.w.(*statusWriter)
	if h.d.Observe != nil {
		h.d.Observe(string(c.op), sw.status, h.d.Now().Sub(c.started))
	}
	if h.d.BytesOut != nil && sw.bytes > 0 && (c.op == OpGetObject) {
		h.d.BytesOut(float64(sw.bytes))
	}
	h.d.Log.Info("request",
		"id", c.id, "op", string(c.op), "method", r.Method, "bucket", c.target.Bucket, "key", c.target.Key,
		"status", sw.status, "bytes", sw.bytes, "dur", h.d.Now().Sub(c.started).Round(time.Microsecond),
		"user", c.caller.Name(), "ip", clientIP(r))
	if h.d.Access != nil {
		rec := usage.Record{
			Time: c.started, ID: c.id, Op: string(c.op), Method: r.Method, Bucket: c.target.Bucket, Key: c.target.Key,
			Status: sw.status, BytesOut: sw.bytes, Duration: h.d.Now().Sub(c.started).Milliseconds(),
			User: c.caller.Name(), IP: clientIP(r), UserAgent: r.UserAgent(), Referer: r.Referer(),
		}
		if r.ContentLength > 0 {
			rec.BytesIn = r.ContentLength
		}
		if c.cred != nil {
			rec.AccessKey = c.cred.Key.AccessKeyID
		} else if c.user != nil {
			rec.AccessKey = "session"
		}
		rec.Replicated = c.caller != nil && r.Header.Get(s3client.OriginHeader) != ""
		if sw.status >= 400 {
			rec.ErrorCode, rec.ErrorMessage = c.errCode, c.errMsg
		}
		h.d.Access(rec)
	}
}

func hostnames(cfg config.Config) []string {
	var out []string
	if cfg.Hostname != "" {
		out = append(out, cfg.Hostname)
	}
	out = append(out, cfg.TLS.Domains...)
	return out
}

func (h *Handler) serve(c *reqCtx) error {
	// Preflight needs no auth; it only consults bucket CORS rules.
	if c.op == OpOptions {
		return h.preflight(c)
	}
	if c.op == OpUnknown {
		return s3err.New(s3err.MethodNotAllowed)
	}
	if err := h.authenticate(c); err != nil {
		return err
	}
	if c.target.Bucket != "" && c.op != OpCreateBucket {
		if !ValidBucketName(c.target.Bucket) && !c.target.VirtualHost {
			return s3err.New(s3err.InvalidBucketName)
		}
		b, err := h.d.DB.GetBucket(c.ctx(), c.target.Bucket)
		if err != nil {
			if errors.Is(err, meta.ErrNoSuchBucket) {
				return s3err.New(s3err.NoSuchBucket)
			}
			return err
		}
		c.bucket = b
		if b.Policy != "" {
			if c.pol, err = policy.Parse([]byte(b.Policy), b.Name); err != nil {
				// Fail closed: an unreadable policy denies everyone but admins.
				c.polBroken = true
			}
		}
		h.applyCORS(c)
	}
	if c.target.Key != "" && !ValidKey(c.target.Key) {
		return s3err.New(s3err.KeyTooLongError)
	}
	if err := h.authorize(c); err != nil {
		return err
	}
	if isWrite(c.op) {
		if err := h.d.Writable(); err != nil {
			return s3err.Newf(s3err.ReadOnly, "%v", err)
		}
	}
	fn, ok := h.ops[c.op]
	if !ok {
		return s3err.New(s3err.NotImplemented)
	}
	return fn(c)
}

func isWrite(op Operation) bool {
	switch op {
	case OpListBuckets, OpHeadBucket, OpListObjects, OpListObjectsV2, OpListObjectVersions, OpListMultipartUploads,
		OpGetBucketLocation, OpGetBucketVersioning, OpGetBucketPolicy, OpGetBucketPolicyStatus, OpGetBucketCors,
		OpGetBucketLifecycle, OpGetBucketAcl, OpGetBucketTagging, OpGetBucketEncryption, OpGetObjectLockConfig,
		OpGetPublicAccessBlock, OpGetBucketOwnership, OpGetBucketLogging, OpGetBucketNotification,
		OpGetBucketReplication,
		OpGetObject, OpHeadObject, OpGetObjectAcl, OpGetObjectTagging, OpGetObjectRetention, OpGetObjectLegalHold,
		OpGetObjectAttributes, OpListParts, OpOptions:
		return false
	}
	return true
}

// authenticate resolves the caller from SigV4 or marks the request anonymous.
func (h *Handler) authenticate(c *reqCtx) error {
	ip := clientIP(c.r)
	if h.limiter.blocked(ip, h.d.Now(), h.d.Cfg().Limits) {
		e := s3err.New(s3err.SlowDown).WithMessage("Too many failed authentication attempts; try again later")
		e.HTTPStatus = http.StatusTooManyRequests
		return e
	}
	ident, err := h.verifier.Verify(c.ctx(), c.r)
	if err != nil {
		if errors.Is(err, auth.ErrMissingAuth) {
			return h.authenticateSession(c, ip)
		}
		h.limiter.fail(ip, h.d.Now())
		if h.d.AuthFailure != nil {
			h.d.AuthFailure()
		}
		return mapAuthError(err)
	}
	cred, err := h.d.DB.LookupAccessKey(c.ctx(), ident.AccessKeyID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchKeyID) {
			h.limiter.fail(ip, h.d.Now())
			return s3err.New(s3err.InvalidAccessKeyID)
		}
		return err
	}
	if cred.Key.Disabled || cred.User.Disabled {
		h.limiter.fail(ip, h.d.Now())
		return s3err.New(s3err.InvalidAccessKeyID).WithMessage("The access key is disabled")
	}
	c.ident = ident
	c.cred = cred
	c.user = &cred.User
	c.caller = &policy.Caller{User: &cred.User}
	return nil
}

// replicated reports whether the request is a replicated change from another
// hogshead (authenticated and carrying the origin header) and returns the
// version id and creation time to reuse. Falls back to fresh values.
func (h *Handler) replicated(c *reqCtx, fallbackVID string) (vid string, now int64) {
	vid, now = fallbackVID, h.nowMs()
	if c.caller == nil || c.r.Header.Get(s3client.OriginHeader) == "" {
		return vid, now
	}
	if v := c.r.Header.Get(s3client.SourceVersionHeader); v != "" && v != meta.NullVersionID && len(v) <= 64 {
		vid = v
	}
	if t := c.r.Header.Get(s3client.SourceTimeHeader); t != "" {
		if ms, err := strconv.ParseInt(t, 10, 64); err == nil && ms > 0 {
			now = ms
		}
	}
	return vid, now
}

// SyncReplicator is the synchronous side of replication (see replication.Worker).
type SyncReplicator interface {
	SyncPut(ctx context.Context, cmd *meta.PutObjectVersion) error
	SyncDelete(ctx context.Context, cmd *meta.DeleteObject, versioned bool) error
}

// SetSync enables synchronous write acknowledgement.
func (h *Handler) SetSync(s SyncReplicator) { h.d.Sync = s }

// SetOnChange registers the hook run after committed object changes.
func (h *Handler) SetOnChange(fn func()) { h.d.OnChange = fn }

// ackWrite pushes a put to cellar members when write acknowledgement is on.
// Replicated writes (carrying an origin) are never pushed again from here.
func (h *Handler) ackWrite(c *reqCtx, cmd *meta.PutObjectVersion) error {
	if h.d.Sync == nil || c.r.Header.Get(s3client.OriginHeader) != "" {
		return nil
	}
	if err := h.d.Sync.SyncPut(c.ctx(), cmd); err != nil {
		return s3err.New(s3err.ServiceUnavailable).WithMessage(err.Error())
	}
	return nil
}

// ackDelete is ackWrite for deletes.
func (h *Handler) ackDelete(c *reqCtx, cmd *meta.DeleteObject) error {
	if h.d.Sync == nil || c.r.Header.Get(s3client.OriginHeader) != "" {
		return nil
	}
	if err := h.d.Sync.SyncDelete(c.ctx(), cmd, c.bucket.Versioning == meta.VersioningEnabled); err != nil {
		return s3err.New(s3err.ServiceUnavailable).WithMessage(err.Error())
	}
	return nil
}

// changed notifies replication of a committed object change.
func (h *Handler) changed() {
	if h.d.OnChange != nil {
		h.d.OnChange()
	}
}

// SetAccess registers the per-request record hook.
func (h *Handler) SetAccess(fn func(usage.Record)) { h.d.Access = fn }

// SetSession enables cookie authentication for web UI requests.
func (h *Handler) SetSession(s SessionAuth) { h.d.Session = s }

// SessionAuth resolves a web UI session cookie to a user. It returns
// (nil, nil) when the request carries no session.
type SessionAuth interface {
	UserFromRequest(ctx context.Context, r *http.Request) (*meta.User, error)
}

// authenticateSession handles requests without SigV4: a valid session cookie
// makes the request act as that user, anything else is anonymous.
func (h *Handler) authenticateSession(c *reqCtx, ip string) error {
	c.caller = nil
	if h.d.Session == nil {
		return nil
	}
	u, err := h.d.Session.UserFromRequest(c.ctx(), c.r)
	if err != nil {
		h.limiter.fail(ip, h.d.Now())
		return s3err.New(s3err.AccessDenied).WithMessage(err.Error())
	}
	if u == nil {
		return nil
	}
	c.user = u
	c.caller = &policy.Caller{User: u}
	return nil
}

func mapAuthError(err error) error {
	switch {
	case errors.Is(err, auth.ErrUnknownKey):
		return s3err.New(s3err.InvalidAccessKeyID)
	case errors.Is(err, auth.ErrSignatureMismatch):
		return s3err.New(s3err.SignatureDoesNotMatch)
	case errors.Is(err, auth.ErrSkew):
		return s3err.New(s3err.RequestTimeTooSkewed)
	case errors.Is(err, auth.ErrExpired):
		return s3err.New(s3err.AccessDenied).WithMessage("Request has expired")
	case errors.Is(err, auth.ErrUnsupported):
		return s3err.New(s3err.InvalidArgument).WithMessage("Unsupported Authorization type")
	case errors.Is(err, auth.ErrMalformed):
		return s3err.Newf(s3err.AuthorizationHeaderMalformed, "%v", err)
	}
	return err
}

func (h *Handler) authorize(c *reqCtx) error {
	if c.polBroken && !c.caller.IsAdmin() {
		return s3err.New(s3err.AccessDenied).WithMessage("The bucket policy could not be evaluated")
	}
	d := h.decide(c, c.op, c.target.Key, c.r.URL.Query().Get("versionId") != "")
	if d.Allowed {
		return nil
	}
	// Objects can carry a canned public-read ACL of their own.
	if (c.op == OpGetObject || c.op == OpHeadObject || c.op == OpGetObjectAcl) && c.bucket != nil && c.target.Key != "" {
		if v, err := h.d.DB.GetObject(c.ctx(), c.bucket.Name, c.target.Key, c.r.URL.Query().Get("versionId")); err == nil && !v.DeleteMarker && v.Metadata[metaACL] == "public-read" {
			return nil
		}
	}
	return s3err.New(s3err.AccessDenied)
}

// decide evaluates roles and the bucket policy for one key.
func (h *Handler) decide(c *reqCtx, op Operation, key string, versioned bool) policy.Decision {
	req := policy.Request{Action: ActionFor(op, versioned), Bucket: c.target.Bucket, Key: key, Context: h.conditionContext(c)}
	owner := ""
	if c.bucket != nil {
		owner = c.bucket.Owner
	}
	return policy.AuthorizeOwned(c.caller, c.pol, req, owner)
}

// conditionContext collects policy condition keys for the request.
func (h *Handler) conditionContext(c *reqCtx) map[string]string {
	q := c.r.URL.Query()
	ctx := map[string]string{
		"aws:SourceIp":                    clientIP(c.r),
		"aws:SecureTransport":             strconv.FormatBool(c.r.TLS != nil),
		"aws:Referer":                     c.r.Header.Get("Referer"),
		"s3:prefix":                       q.Get("prefix"),
		"s3:delimiter":                    q.Get("delimiter"),
		"s3:max-keys":                     q.Get("max-keys"),
		"s3:VersionId":                    q.Get("versionId"),
		"s3:x-amz-acl":                    c.r.Header.Get("x-amz-acl"),
		"s3:x-amz-storage-class":          c.r.Header.Get("x-amz-storage-class"),
		"s3:x-amz-metadata-directive":     c.r.Header.Get("x-amz-metadata-directive"),
		"s3:x-amz-copy-source":            strings.TrimPrefix(c.r.Header.Get("x-amz-copy-source"), "/"),
		"s3:x-amz-server-side-encryption": c.r.Header.Get("x-amz-server-side-encryption"),
		"s3:authType":                     "REST-HEADER",
		"s3:signatureversion":             "AWS4-HMAC-SHA256",
	}
	if c.ident != nil && c.ident.Presigned {
		ctx["s3:authType"] = "REST-QUERY-STRING"
	}
	if c.ident != nil && c.ident.Version == 2 {
		ctx["s3:signatureversion"] = "AWS"
	}
	if c.user != nil {
		ctx["aws:username"] = c.user.Name
		ctx["aws:userid"] = c.user.ID
		ctx["aws:PrincipalType"] = "User"
	} else {
		ctx["aws:PrincipalType"] = "Anonymous"
	}
	for k, v := range ctx {
		if v == "" {
			delete(ctx, k)
		}
	}
	return ctx
}

type opFunc func(c *reqCtx) error

func (h *Handler) buildDispatch() map[Operation]opFunc {
	return map[Operation]opFunc{
		OpListBuckets:             h.listBuckets,
		OpCreateBucket:            h.createBucket,
		OpDeleteBucket:            h.deleteBucket,
		OpHeadBucket:              h.headBucket,
		OpGetBucketLocation:       h.getBucketLocation,
		OpGetBucketVersioning:     h.getBucketVersioning,
		OpPutBucketVersioning:     h.putBucketVersioning,
		OpGetBucketPolicy:         h.getBucketPolicy,
		OpPutBucketPolicy:         h.putBucketPolicy,
		OpDeleteBucketPolicy:      h.deleteBucketPolicy,
		OpGetBucketPolicyStatus:   h.getBucketPolicyStatus,
		OpGetBucketCors:           h.getBucketCors,
		OpPutBucketCors:           h.putBucketCors,
		OpDeleteBucketCors:        h.deleteBucketCors,
		OpGetBucketLifecycle:      h.getBucketLifecycle,
		OpPutBucketLifecycle:      h.putBucketLifecycle,
		OpDeleteBucketLifecycle:   h.deleteBucketLifecycle,
		OpGetBucketAcl:            h.getBucketAcl,
		OpPutBucketAcl:            h.putBucketAcl,
		OpGetBucketTagging:        h.getBucketTagging,
		OpPutBucketTagging:        h.putBucketTagging,
		OpDeleteBucketTagging:     h.deleteBucketTagging,
		OpGetBucketEncryption:     h.getBucketEncryption,
		OpPutBucketEncryption:     h.ok200,
		OpDeleteBucketEncryption:  h.noContent,
		OpGetObjectLockConfig:     h.getObjectLockConfig,
		OpPutObjectLockConfig:     h.putObjectLockConfig,
		OpGetPublicAccessBlock:    h.notFound("NoSuchPublicAccessBlockConfiguration", "The public access block configuration was not found"),
		OpPutPublicAccessBlock:    h.ok200,
		OpDeletePublicAccessBlock: h.noContent,
		OpGetBucketOwnership:      h.notFound("OwnershipControlsNotFoundError", "The bucket ownership controls were not found"),
		OpPutBucketOwnership:      h.ok200,
		OpGetBucketLogging:        h.xmlConst(&s3xml.BucketLoggingStatus{}),
		OpGetBucketNotification:   h.getBucketNotification,
		OpPutBucketNotification:   h.putBucketNotification,
		OpGetBucketReplication:    h.notFound("ReplicationConfigurationNotFoundError", "The replication configuration was not found"),
		OpListObjects:             h.listObjectsV1,
		OpListObjectsV2:           h.listObjectsV2,
		OpListObjectVersions:      h.listObjectVersions,
		OpListMultipartUploads:    h.listMultipartUploads,
		OpDeleteObjects:           h.deleteObjects,
		OpGetObject:               h.getObject,
		OpHeadObject:              h.getObject,
		OpPutObject:               h.putObject,
		OpCopyObject:              h.copyObject,
		OpDeleteObject:            h.deleteObject,
		OpGetObjectAcl:            h.getObjectAcl,
		OpPutObjectAcl:            h.putObjectAcl,
		OpGetObjectTagging:        h.getObjectTagging,
		OpPutObjectTagging:        h.putObjectTagging,
		OpDeleteObjectTagging:     h.deleteObjectTagging,
		OpGetObjectRetention:      h.getObjectRetention,
		OpPutObjectRetention:      h.putObjectRetention,
		OpGetObjectLegalHold:      h.getObjectLegalHold,
		OpPutObjectLegalHold:      h.putObjectLegalHold,
		OpGetObjectAttributes:     h.getObjectAttributes,
		OpCreateMultipart:         h.createMultipartUpload,
		OpUploadPart:              h.uploadPart,
		OpUploadPartCopy:          h.uploadPartCopy,
		OpCompleteMultipart:       h.completeMultipartUpload,
		OpAbortMultipart:          h.abortMultipartUpload,
		OpListParts:               h.listParts,
	}
}

// ---- response helpers ----

func (c *reqCtx) xml(status int, v any) error {
	body, err := s3xml.Marshal(v)
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "application/xml")
	c.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	c.w.WriteHeader(status)
	if c.r.Method != http.MethodHead {
		_, _ = c.w.Write(body)
	}
	return nil
}

func (c *reqCtx) empty(status int) error {
	if status == http.StatusOK || status == http.StatusNoContent {
		c.w.Header().Set("Content-Length", "0")
	}
	c.w.WriteHeader(status)
	return nil
}

func (h *Handler) writeError(c *reqCtx, err error) {
	e := toS3Error(err)
	if e.HTTPStatus >= 500 && e.Code != s3err.ReadOnly && e.Code != s3err.ServiceUnavailable {
		h.d.Log.Error("request failed", "id", c.id, "op", string(c.op), "err", err)
	}
	c.errCode, c.errMsg = string(e.Code), e.Message
	e = e.WithRequestID(c.id)
	res := "/" + c.target.Bucket
	if c.target.Key != "" {
		res += "/" + c.target.Key
	}
	e = e.WithResource(res)
	if e.Code == s3err.NotModified {
		c.w.WriteHeader(http.StatusNotModified)
		return
	}
	body := e.XML()
	c.w.Header().Set("Content-Type", "application/xml")
	c.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	c.w.WriteHeader(e.HTTPStatus)
	if c.r.Method != http.MethodHead {
		_, _ = c.w.Write(body)
	}
}

// toS3Error maps internal errors onto S3 error codes.
func toS3Error(err error) *s3err.Error {
	var se *s3err.Error
	if errors.As(err, &se) {
		return se
	}
	var me *meta.Error
	if errors.As(err, &me) {
		code := s3err.Code(me.Code)
		if s3err.IsRegistered(code) {
			e := s3err.New(code)
			if me.Msg != "" {
				e = e.WithMessage(me.Msg)
			}
			return e
		}
		return s3err.Newf(s3err.InternalError, "%s", me.Error())
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return s3err.New(s3err.InternalError).WithMessage("Object data is missing on this node")
	case errors.Is(err, store.ErrTooLarge):
		return s3err.New(s3err.EntityTooLarge)
	case errors.Is(err, ErrMetadataTooLarge):
		return s3err.New(s3err.MetadataTooLarge)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return s3err.New(s3err.RequestTimeout)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return s3err.New(s3err.RequestTimeout)
	}
	return s3err.New(s3err.InternalError)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusWriter records status and bytes for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush supports streaming responses.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---- failed-auth rate limiting ----

type authLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func newAuthLimiter() *authLimiter { return &authLimiter{fails: map[string][]time.Time{}} }

func (l *authLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.fails[ip], now)
	if len(l.fails) > 10000 {
		// Crude memory bound: drop everything, attackers restart from zero.
		l.fails = map[string][]time.Time{}
	}
}

func (l *authLimiter) blocked(ip string, now time.Time, lim config.Limits) bool {
	if lim.AuthFailBurst <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	window := lim.AuthFailWindow.Std()
	times := l.fails[ip]
	keep := times[:0]
	for _, t := range times {
		if now.Sub(t) < window {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = keep
	}
	return len(keep) >= lim.AuthFailBurst
}

// ---- small shared helpers ----

func (h *Handler) ok200(c *reqCtx) error     { return c.empty(http.StatusOK) }
func (h *Handler) noContent(c *reqCtx) error { return c.empty(http.StatusNoContent) }

func (h *Handler) notFound(code, msg string) opFunc {
	return func(c *reqCtx) error {
		return &s3err.Error{Code: s3err.Code(code), Message: msg, HTTPStatus: http.StatusNotFound}
	}
}

func (h *Handler) xmlConst(v any) opFunc {
	return func(c *reqCtx) error { return c.xml(http.StatusOK, v) }
}

func (h *Handler) now() time.Time { return h.d.Now() }

func (h *Handler) nowMs() int64 { return h.d.Now().UnixMilli() }

// propose applies a command and maps results.
func (h *Handler) propose(c *reqCtx, cmd meta.Command) (meta.Result, error) {
	// A change pushed by another hogshead server's replication worker carries
	// its origin; recording it keeps the change from being forwarded again.
	if o := c.r.Header.Get(s3client.OriginHeader); o != "" && c.caller != nil {
		wrapped, err := meta.Wrap(o, cmd)
		if err != nil {
			return meta.Result{}, err
		}
		cmd = wrapped
	}
	res, err := h.d.Propose(c.ctx(), cmd)
	if err != nil {
		return res, err
	}
	if res.Err != "" {
		return res, res.Error()
	}
	return res, nil
}

// owner returns the XML owner for a user id.
func (h *Handler) owner(c *reqCtx, id string) *s3xml.Owner {
	name := id
	if c.user != nil && c.user.ID == id {
		name = c.user.Name
	} else if u, err := h.d.DB.GetUser(c.ctx(), id); err == nil {
		name = u.Name
	}
	return &s3xml.Owner{ID: id, DisplayName: name}
}

func (h *Handler) callerID(c *reqCtx) string {
	if c.user != nil {
		return c.user.ID
	}
	return ""
}

func versionedHeader(c *reqCtx, vid string) {
	if c.bucket != nil && (c.bucket.Versioning != meta.VersioningOff || vid != meta.NullVersionID) {
		c.w.Header().Set("x-amz-version-id", vid)
	}
}

func readBodyLimited(c *reqCtx, limit int64) ([]byte, error) {
	if c.r.ContentLength > limit {
		return nil, s3err.New(s3err.MaxMessageLengthExceeded)
	}
	b, err := io.ReadAll(io.LimitReader(c.r.Body, limit+1))
	if err != nil {
		return nil, s3err.New(s3err.IncompleteBody)
	}
	if int64(len(b)) > limit {
		return nil, s3err.New(s3err.MaxMessageLengthExceeded)
	}
	return b, nil
}

func parseXMLBody(c *reqCtx, v any) error {
	return s3xml.Unmarshal(c.r.Body, v, s3xml.DefaultLimit)
}
