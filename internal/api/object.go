package api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/enetforge/hogshead/internal/api/s3err"
	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/policy"
	"github.com/enetforge/hogshead/internal/store"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

var _ = url.PathEscape

// Metadata keys used for system state stored alongside user metadata.
const (
	metaTagging        = "x-amz-tagging"
	metaLockMode       = "x-amz-object-lock-mode"
	metaLockUntil      = "x-amz-object-lock-retain-until-date"
	metaLegalHold      = "x-amz-object-lock-legal-hold"
	metaPartSizes      = "x-hogshead-mp-parts"
	metaACL            = "x-amz-acl"
	defaultContentType = "binary/octet-stream"
)

var validStorageClasses = map[string]bool{
	"": true, "STANDARD": true, "REDUCED_REDUNDANCY": true, "STANDARD_IA": true, "ONEZONE_IA": true,
	"INTELLIGENT_TIERING": true, "GLACIER": true, "DEEP_ARCHIVE": true, "GLACIER_IR": true, "EXPRESS_ONEZONE": true,
}

func (h *Handler) lookupObject(c *reqCtx, versionID string) (*meta.ObjectVersion, error) {
	v, err := h.d.DB.GetObject(c.ctx(), c.bucket.Name, c.target.Key, versionID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchKey) {
			return nil, s3err.New(s3err.NoSuchKey)
		}
		if errors.Is(err, meta.ErrNoSuchVersion) {
			return nil, s3err.New(s3err.NoSuchVersion)
		}
		return nil, err
	}
	if v.DeleteMarker {
		c.w.Header().Set("x-amz-delete-marker", "true")
		versionedHeader(c, v.VersionID)
		if versionID == "" {
			return nil, s3err.New(s3err.NoSuchKey)
		}
		return nil, s3err.New(s3err.MethodNotAllowed)
	}
	return v, nil
}

func objectLockHeaders(c *reqCtx, m map[string]string) {
	for _, k := range []string{metaLockMode, metaLockUntil, metaLegalHold} {
		if v := m[k]; v != "" {
			c.w.Header().Set(k, v)
		}
	}
}

func partSizes(m map[string]string) []int64 {
	s := m[metaPartSizes]
	if s == "" {
		return nil
	}
	var out []int64
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func (h *Handler) getObject(c *reqCtx) error {
	q := c.r.URL.Query()
	v, err := h.lookupObject(c, q.Get("versionId"))
	if err != nil {
		return err
	}
	hdr := c.w.Header()
	etag := QuoteETag(v.ETag)
	switch EvaluateConditions(c.r.Header, etag, v.Created) {
	case CondNotModified:
		hdr.Set("ETag", etag)
		hdr.Set("Last-Modified", FormatHTTPTime(v.Created))
		return s3err.New(s3err.NotModified)
	case CondPreconditionFailed:
		return s3err.New(s3err.PreconditionFailed)
	}
	ApplyMetadata(hdr, v.Metadata)
	if hdr.Get("Content-Type") == "" {
		ct := v.ContentType
		if ct == "" {
			ct = defaultContentType
		}
		hdr.Set("Content-Type", ct)
	}
	hdr.Set("ETag", etag)
	hdr.Set("Last-Modified", FormatHTTPTime(v.Created))
	versionedHeader(c, v.VersionID)
	if v.StorageClass != "" && v.StorageClass != "STANDARD" {
		hdr.Set("x-amz-storage-class", v.StorageClass)
	}
	if t := v.Metadata[metaTagging]; t != "" {
		if tags, err := parseTagQuery(t); err == nil {
			hdr.Set("x-amz-tagging-count", strconv.Itoa(len(tags)))
		}
	}
	objectLockHeaders(c, v.Metadata)
	sizes := partSizes(v.Metadata)
	if len(sizes) > 0 {
		hdr.Set("x-amz-mp-parts-count", strconv.Itoa(len(sizes)))
	}
	if !c.caller.Anonymous() {
		for q, hn := range map[string]string{
			"response-content-type": "Content-Type", "response-content-language": "Content-Language",
			"response-expires": "Expires", "response-cache-control": "Cache-Control",
			"response-content-disposition": "Content-Disposition", "response-content-encoding": "Content-Encoding",
		} {
			if val := c.r.URL.Query().Get(q); val != "" {
				hdr.Set(hn, val)
			}
		}
	}

	off, length := int64(0), v.Size
	status := http.StatusOK
	if pn := q.Get("partNumber"); pn != "" {
		n, perr := strconv.Atoi(pn)
		if perr != nil || n < 1 || n > 10000 {
			return s3err.New(s3err.InvalidArgument).WithMessage("Part number must be an integer between 1 and 10000")
		}
		if len(sizes) == 0 {
			if n != 1 {
				return s3err.New(s3err.InvalidPartNumber)
			}
		} else {
			if n > len(sizes) {
				return s3err.New(s3err.InvalidPartNumber)
			}
			for i := 0; i < n-1; i++ {
				off += sizes[i]
			}
			length = sizes[n-1]
			status = http.StatusPartialContent
			hdr.Set("Content-Range", ContentRange(off, length, v.Size))
		}
	} else if rh := c.r.Header.Get("Range"); rh != "" {
		o, l, ok, rerr := ParseRange(rh, v.Size)
		if rerr != nil {
			hdr.Set("Content-Range", "bytes */"+strconv.FormatInt(v.Size, 10))
			return s3err.New(s3err.InvalidRange)
		}
		if ok {
			off, length = o, l
			status = http.StatusPartialContent
			hdr.Set("Content-Range", ContentRange(off, length, v.Size))
		}
	}
	// Checksums describe the whole object; AWS omits them on partial responses.
	if status == http.StatusOK && strings.EqualFold(c.r.Header.Get("x-amz-checksum-mode"), "ENABLED") {
		setChecksumHeaders(hdr, v.Checksums)
	}
	hdr.Set("Content-Length", strconv.FormatInt(length, 10))
	if c.r.Method == http.MethodHead || length == 0 {
		c.w.WriteHeader(status)
		return nil
	}
	rc, err := store.Open(c.ctx(), h.d.Chunks, v.Chunks, off, length)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	c.w.WriteHeader(status)
	if _, err := io.Copy(c.w, rc); err != nil {
		// Headers are already sent; log and drop the connection.
		h.d.Log.Warn("object stream aborted", "id", c.id, "key", v.Key, "err", err)
	}
	return nil
}

func (h *Handler) objectMetadataFromRequest(c *reqCtx) (map[string]string, error) {
	m, err := ExtractMetadata(c.r.Header)
	if err != nil {
		return nil, err
	}
	if t := c.r.Header.Get("x-amz-tagging"); t != "" {
		if _, err := parseTagQuery(t); err != nil {
			return nil, err
		}
		m[metaTagging] = t
	}
	if sc := c.r.Header.Get("x-amz-storage-class"); !validStorageClasses[sc] {
		return nil, s3err.New(s3err.InvalidStorageClass)
	}
	if err := h.lockFromRequest(c, m); err != nil {
		return nil, err
	}
	if acl, err := cannedObjectACL(c.r.Header); err != nil {
		return nil, err
	} else if acl != "" {
		m[metaACL] = acl
	}
	return m, nil
}

// cannedObjectACL maps x-amz-acl / x-amz-grant-read headers to the one object
// ACL we support: "public-read". Private is the default.
func cannedObjectACL(h http.Header) (string, error) {
	switch acl := h.Get("x-amz-acl"); acl {
	case "", "private", "bucket-owner-read", "bucket-owner-full-control", "authenticated-read", "aws-exec-read":
	case "public-read", "public-read-write":
		return "public-read", nil
	default:
		return "", s3err.New(s3err.InvalidArgument).WithMessage("Unknown canned ACL")
	}
	if strings.Contains(h.Get("x-amz-grant-read"), "AllUsers") || strings.Contains(h.Get("x-amz-grant-full-control"), "AllUsers") {
		return "public-read", nil
	}
	return "", nil
}

// aclFromBody parses an AccessControlPolicy document into a canned ACL.
func aclFromBody(c *reqCtx) (string, error) {
	var acp s3xml.AccessControlPolicy
	if err := parseXMLBody(c, &acp); err != nil {
		return "", err
	}
	read, write := false, false
	for _, g := range acp.AccessControlList {
		if !strings.HasSuffix(g.Grantee.URI, "/AllUsers") {
			continue
		}
		switch g.Permission {
		case "READ", "FULL_CONTROL":
			read = true
			if g.Permission == "FULL_CONTROL" {
				write = true
			}
		case "WRITE":
			write = true
		}
	}
	switch {
	case read && write:
		return "public-read-write", nil
	case read:
		return "public-read", nil
	case write:
		return "public-read-write", nil
	}
	return "private", nil
}

// aclDocument renders an ACL with the owner and any public grants.
func (h *Handler) aclDocument(c *reqCtx, ownerID, canned string) s3xml.AccessControlPolicy {
	acl := s3xml.FullControlACL(*h.owner(c, ownerID))
	grant := func(perm string) {
		acl.AccessControlList = append(acl.AccessControlList, s3xml.Grant{
			Grantee:    s3xml.Grantee{XmlnsXsi: s3xml.XsiNS, Type: "Group", URI: "http://acs.amazonaws.com/groups/global/AllUsers"},
			Permission: perm,
		})
	}
	switch canned {
	case "public-read":
		grant("READ")
	case "public-read-write":
		grant("READ")
		grant("WRITE")
	}
	return acl
}

func (h *Handler) lockFromRequest(c *reqCtx, m map[string]string) error {
	mode := c.r.Header.Get(metaLockMode)
	until := c.r.Header.Get(metaLockUntil)
	hold := c.r.Header.Get(metaLegalHold)
	if mode == "" && until == "" && c.bucket.ObjectLock {
		// Apply the bucket's default retention rule, if configured.
		if dr := h.defaultRetention(c); dr != nil {
			t := h.now().UTC()
			switch {
			case dr.Years != nil && *dr.Years > 0:
				t = t.AddDate(*dr.Years, 0, 0)
			case dr.Days != nil:
				t = t.AddDate(0, 0, *dr.Days)
			}
			m[metaLockMode], m[metaLockUntil] = dr.Mode, t.Format(time.RFC3339)
		}
	}
	if mode == "" && until == "" && hold == "" {
		return nil
	}
	if !c.bucket.ObjectLock {
		return s3err.New(s3err.InvalidRequest).WithMessage("Bucket is missing Object Lock Configuration")
	}
	if (mode == "") != (until == "") {
		return s3err.New(s3err.InvalidArgument).WithMessage("x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied")
	}
	if mode != "" {
		if mode != "GOVERNANCE" && mode != "COMPLIANCE" {
			return s3err.New(s3err.InvalidArgument).WithMessage("Unknown object lock mode")
		}
		t, err := time.Parse(time.RFC3339, until)
		if err != nil || !t.After(h.now()) {
			return s3err.New(s3err.InvalidArgument).WithMessage("The retain until date must be in the future")
		}
		m[metaLockMode], m[metaLockUntil] = mode, t.UTC().Format(time.RFC3339)
	}
	if hold != "" {
		if hold != "ON" && hold != "OFF" {
			return s3err.New(s3err.InvalidArgument).WithMessage("Legal hold must be ON or OFF")
		}
		m[metaLegalHold] = hold
	}
	return nil
}

func (h *Handler) putObject(c *reqCtx) error {
	m, err := h.objectMetadataFromRequest(c)
	if err != nil {
		return err
	}
	res, err := h.ingest(c, nil, h.d.Cfg().Limits.MaxObjectSize)
	if err != nil {
		return err
	}
	vid, now := h.replicated(c, ids.VersionID())
	cmd := &meta.PutObjectVersion{
		Bucket: c.bucket.Name, Key: c.target.Key, VersionID: vid,
		Size: res.Size, ETag: res.MD5, ContentType: m["content-type"], Metadata: m,
		Chunks: res.Chunks, Checksums: res.Checksums, StorageClass: c.r.Header.Get("x-amz-storage-class"),
		Owner: h.callerID(c), Now: now,
		IfNoneMatchAny: c.r.Header.Get("If-None-Match") == "*",
		IfMatchETag:    c.r.Header.Get("If-Match"),
	}
	if inm := c.r.Header.Get("If-None-Match"); inm != "" && inm != "*" {
		cmd.IfNoneMatchETag = inm
	}
	if err := h.ackWrite(c, cmd); err != nil {
		return err
	}
	r, err := h.propose(c, cmd)
	if err != nil {
		return err
	}
	h.changed()
	var pr meta.PutObjectResult
	_ = r.Decode(&pr)
	h.emit(c, "s3:ObjectCreated:Put", c.target.Key, res.Size, res.MD5, pr.VersionID)
	c.w.Header().Set("ETag", QuoteETag(res.MD5))
	versionedHeader(c, pr.VersionID)
	setChecksumHeaders(c.w.Header(), res.Checksums)
	return c.empty(http.StatusOK)
}

func (h *Handler) copyObject(c *reqCtx) error {
	srcBucket, srcKey, srcVid, err := CopySource(c.r.Header.Get("x-amz-copy-source"))
	if err != nil {
		return s3err.New(s3err.InvalidArgument).WithMessage("Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	src, err := h.loadSource(c, srcBucket, srcKey, srcVid)
	if err != nil {
		return err
	}
	ch := http.Header{}
	for _, k := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if v := c.r.Header.Get("x-amz-copy-source-" + strings.ToLower(k)); v != "" {
			ch.Set(k, v)
		}
	}
	if EvaluateConditions(ch, QuoteETag(src.ETag), src.Created) != CondOK {
		return s3err.New(s3err.PreconditionFailed)
	}
	directive := strings.ToUpper(c.r.Header.Get("x-amz-metadata-directive"))
	m := map[string]string{}
	switch directive {
	case "", "COPY":
		for k, v := range src.Metadata {
			m[k] = v
		}
		if srcBucket == c.bucket.Name && srcKey == c.target.Key && c.r.Header.Get("x-amz-storage-class") == "" && strings.ToUpper(c.r.Header.Get("x-amz-tagging-directive")) != "REPLACE" {
			return s3err.New(s3err.InvalidRequest).WithMessage("This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.")
		}
	case "REPLACE":
		if m, err = ExtractMetadata(c.r.Header); err != nil {
			return err
		}
		if t := src.Metadata[metaTagging]; t != "" {
			m[metaTagging] = t
		}
	default:
		return s3err.New(s3err.InvalidArgument).WithMessage("Unknown metadata directive")
	}
	if strings.ToUpper(c.r.Header.Get("x-amz-tagging-directive")) == "REPLACE" {
		delete(m, metaTagging)
		if t := c.r.Header.Get("x-amz-tagging"); t != "" {
			m[metaTagging] = t
		}
	}
	delete(m, metaPartSizes)
	for _, k := range []string{metaLockMode, metaLockUntil, metaLegalHold} {
		delete(m, k)
	}
	if err := h.lockFromRequest(c, m); err != nil {
		return err
	}
	sc := c.r.Header.Get("x-amz-storage-class")
	if !validStorageClasses[sc] {
		return s3err.New(s3err.InvalidStorageClass)
	}
	if sc == "" {
		sc = src.StorageClass
	}
	ct := m["content-type"]
	if ct == "" {
		ct = src.ContentType
	}
	vid, now := h.replicated(c, ids.VersionID())
	cmd := &meta.PutObjectVersion{
		Bucket: c.bucket.Name, Key: c.target.Key, VersionID: vid,
		Size: src.Size, ETag: src.ETag, ContentType: ct, Metadata: m, Chunks: src.Chunks,
		Checksums: src.Checksums, StorageClass: sc, Owner: h.callerID(c), Now: now,
	}
	if err := h.ackWrite(c, cmd); err != nil {
		return err
	}
	r, err := h.propose(c, cmd)
	if err != nil {
		return err
	}
	h.changed()
	var pr meta.PutObjectResult
	_ = r.Decode(&pr)
	h.emit(c, "s3:ObjectCreated:Copy", c.target.Key, src.Size, src.ETag, pr.VersionID)
	if src.VersionID != meta.NullVersionID {
		c.w.Header().Set("x-amz-copy-source-version-id", src.VersionID)
	}
	versionedHeader(c, pr.VersionID)
	out := s3xml.CopyObjectResult{LastModified: s3xml.FormatTime(time.UnixMilli(cmd.Now).UTC()), ETag: QuoteETag(src.ETag)}
	out.Checksums = xmlChecksums(src.Checksums)
	return c.xml(http.StatusOK, &out)
}

// loadSource authorizes and fetches a copy source object.
func (h *Handler) loadSource(c *reqCtx, bucket, key, vid string) (*meta.ObjectVersion, error) {
	b, err := h.d.DB.GetBucket(c.ctx(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchBucket) {
			return nil, s3err.New(s3err.NoSuchBucket)
		}
		return nil, err
	}
	var pol *policy.Policy
	if b.Policy != "" {
		pol, _ = policy.Parse([]byte(b.Policy), b.Name)
	}
	if !policy.Authorize(c.caller, pol, policy.Request{Action: "s3:GetObject", Bucket: bucket, Key: key}).Allowed {
		return nil, s3err.New(s3err.AccessDenied)
	}
	v, err := h.d.DB.GetObject(c.ctx(), bucket, key, vid)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchKey) || errors.Is(err, meta.ErrNoSuchVersion) {
			return nil, s3err.New(s3err.NoSuchKey)
		}
		return nil, err
	}
	if v.DeleteMarker {
		return nil, s3err.New(s3err.NoSuchKey)
	}
	return v, nil
}

func xmlChecksums(c meta.Checksums) s3xml.Checksums {
	return s3xml.Checksums{ChecksumCRC32: c.CRC32, ChecksumCRC32C: c.CRC32C, ChecksumSHA1: c.SHA1, ChecksumSHA256: c.SHA256, ChecksumCRC64NVME: c.CRC64NVME}
}

// lockBlocksDelete reports whether Object Lock prevents removing a version.
func (h *Handler) lockBlocksDelete(c *reqCtx, key, vid string) error {
	if !c.bucket.ObjectLock || vid == "" {
		return nil
	}
	v, err := h.d.DB.GetObject(c.ctx(), c.bucket.Name, key, vid)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchKey) || errors.Is(err, meta.ErrNoSuchVersion) {
			return nil
		}
		return err // fail closed on storage errors
	}
	if v.DeleteMarker {
		return nil
	}
	if v.Metadata[metaLegalHold] == "ON" {
		return s3err.New(s3err.AccessDenied).WithMessage("Object is under legal hold")
	}
	if until := v.Metadata[metaLockUntil]; until != "" {
		if t, perr := time.Parse(time.RFC3339, until); perr == nil && t.After(h.now()) {
			if !h.mayBypassGovernance(c, v.Metadata[metaLockMode]) {
				return s3err.New(s3err.AccessDenied).WithMessage("Object is protected by retention until " + until)
			}
		}
	}
	return nil
}

// mayBypassGovernance reports whether the request bypasses GOVERNANCE
// retention: the header must be set and the caller must be an admin, the
// bucket owner, or granted s3:BypassGovernanceRetention by policy.
func (h *Handler) mayBypassGovernance(c *reqCtx, mode string) bool {
	if mode != "GOVERNANCE" || !strings.EqualFold(c.r.Header.Get("x-amz-bypass-governance-retention"), "true") {
		return false
	}
	req := policy.Request{Action: "s3:BypassGovernanceRetention", Bucket: c.bucket.Name, Key: c.target.Key, Context: h.conditionContext(c)}
	return policy.AuthorizeOwned(c.caller, c.pol, req, c.bucket.Owner).Allowed
}

func (h *Handler) deleteObject(c *reqCtx) error {
	vid := c.r.URL.Query().Get("versionId")
	if err := h.lockBlocksDelete(c, c.target.Key, vid); err != nil {
		return err
	}
	markerVID, now := h.replicated(c, ids.VersionID())
	del := &meta.DeleteObject{Bucket: c.bucket.Name, Key: c.target.Key, VersionID: vid, MarkerVersionID: markerVID, Now: now}
	if err := h.ackDelete(c, del); err != nil {
		return err
	}
	r, err := h.propose(c, del)
	if err != nil {
		return err
	}
	h.changed()
	var dr meta.DeleteObjectResult
	_ = r.Decode(&dr)
	if dr.DeleteMarker {
		h.emit(c, "s3:ObjectRemoved:DeleteMarkerCreated", c.target.Key, 0, "", dr.VersionID)
	} else if dr.Existed {
		h.emit(c, "s3:ObjectRemoved:Delete", c.target.Key, 0, "", vid)
	}
	if dr.DeleteMarker {
		c.w.Header().Set("x-amz-delete-marker", "true")
	}
	if dr.VersionID != "" {
		versionedHeader(c, dr.VersionID)
	}
	return c.empty(http.StatusNoContent)
}

func (h *Handler) deleteObjects(c *reqCtx) error {
	var req s3xml.Delete
	if err := parseXMLBody(c, &req); err != nil {
		return err
	}
	if len(req.Object) == 0 || len(req.Object) > 1000 {
		return s3err.New(s3err.MalformedXML).WithMessage("The XML you provided was not well-formed or did not validate against our published schema")
	}
	out := s3xml.DeleteResult{Deleted: []s3xml.DeletedObject{}, Error: []s3xml.DeleteError{}}
	cmd := &meta.DeleteObjects{Bucket: c.bucket.Name, Now: h.nowMs()}
	var idx []int // request index for each command item
	for i, o := range req.Object {
		if !ValidKey(o.Key) {
			out.Error = append(out.Error, s3xml.DeleteError{Key: o.Key, VersionID: o.VersionID, Code: "InvalidArgument", Message: "Invalid key"})
			continue
		}
		if !h.decide(c, OpDeleteObject, o.Key, o.VersionID != "").Allowed {
			out.Error = append(out.Error, s3xml.DeleteError{Key: o.Key, VersionID: o.VersionID, Code: "AccessDenied", Message: "Access Denied"})
			continue
		}
		if err := h.lockBlocksDelete(c, o.Key, o.VersionID); err != nil {
			e := toS3Error(err)
			out.Error = append(out.Error, s3xml.DeleteError{Key: o.Key, VersionID: o.VersionID, Code: string(e.Code), Message: e.Message})
			continue
		}
		cmd.Items = append(cmd.Items, meta.DeleteObject{Key: o.Key, VersionID: o.VersionID, MarkerVersionID: ids.VersionID()})
		idx = append(idx, i)
	}
	if len(cmd.Items) > 0 {
		r, err := h.propose(c, cmd)
		if err != nil {
			return err
		}
		h.changed()
		var res meta.DeleteObjectsResult
		_ = r.Decode(&res)
		for i, ir := range res.Results {
			o := req.Object[idx[i]]
			if ir.Err != "" {
				out.Error = append(out.Error, s3xml.DeleteError{Key: o.Key, VersionID: o.VersionID, Code: string(ir.Err), Message: ir.Msg})
				continue
			}
			if req.Quiet {
				continue
			}
			d := s3xml.DeletedObject{Key: o.Key, VersionID: o.VersionID}
			if ir.DeleteMarker {
				d.DeleteMarker = true
				d.DeleteMarkerVersionID = ir.VersionID
				h.emit(c, "s3:ObjectRemoved:DeleteMarkerCreated", o.Key, 0, "", ir.VersionID)
			} else if ir.Existed {
				h.emit(c, "s3:ObjectRemoved:Delete", o.Key, 0, "", o.VersionID)
			}
			out.Deleted = append(out.Deleted, d)
		}
	}
	return c.xml(http.StatusOK, &out)
}

func (h *Handler) getObjectAcl(c *reqCtx) error {
	v, err := h.lookupObject(c, c.r.URL.Query().Get("versionId"))
	if err != nil {
		return err
	}
	acl := h.aclDocument(c, v.Owner, v.Metadata[metaACL])
	versionedHeader(c, v.VersionID)
	return c.xml(http.StatusOK, &acl)
}

// putObjectAcl supports private and public-read (header, grant header or
// XML body with an AllUsers grant).
func (h *Handler) putObjectAcl(c *reqCtx) error {
	vid := c.r.URL.Query().Get("versionId")
	v, err := h.lookupObject(c, vid)
	if err != nil {
		return err
	}
	acl, err := cannedObjectACL(c.r.Header)
	if err != nil {
		return err
	}
	if acl == "" && c.r.Header.Get("x-amz-acl") == "" && c.r.ContentLength != 0 {
		body, err := aclFromBody(c)
		if err != nil {
			return err
		}
		if body != "private" {
			acl = "public-read"
		}
	}
	cmd := &meta.UpdateObjectMeta{Bucket: c.bucket.Name, Key: c.target.Key, VersionID: v.VersionID}
	if acl == "" {
		cmd.Remove = []string{metaACL}
	} else {
		cmd.Set = map[string]string{metaACL: acl}
	}
	if _, err := h.propose(c, cmd); err != nil {
		return err
	}
	versionedHeader(c, v.VersionID)
	return c.empty(http.StatusOK)
}

// parseTagQuery parses "k=v&k2=v2" preserving order and validating limits.
func parseTagQuery(s string) ([]s3xml.Tag, error) {
	var out []s3xml.Tag
	seen := map[string]bool{}
	for _, pair := range strings.Split(s, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		uk, err1 := url.QueryUnescape(k)
		uv, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil || uk == "" || len(uk) > 128 || len(uv) > 256 || seen[uk] {
			return nil, s3err.New(s3err.InvalidTag)
		}
		seen[uk] = true
		out = append(out, s3xml.Tag{Key: uk, Value: uv})
	}
	if len(out) > 10 {
		return nil, s3err.New(s3err.InvalidTag).WithMessage("Object tags cannot be greater than 10")
	}
	return out, nil
}

func encodeTagQuery(tags []s3xml.Tag) string {
	parts := make([]string, 0, len(tags))
	for _, t := range tags {
		parts = append(parts, url.QueryEscape(t.Key)+"="+url.QueryEscape(t.Value))
	}
	return strings.Join(parts, "&")
}

func parseTagging(s string) s3xml.Tagging {
	t := s3xml.Tagging{TagSet: []s3xml.Tag{}}
	tags, err := parseTagQuery(s)
	if err == nil {
		t.TagSet = tags
	}
	return t
}

func (h *Handler) getObjectTagging(c *reqCtx) error {
	v, err := h.lookupObject(c, c.r.URL.Query().Get("versionId"))
	if err != nil {
		return err
	}
	versionedHeader(c, v.VersionID)
	t := parseTagging(v.Metadata[metaTagging])
	return c.xml(http.StatusOK, &t)
}

func (h *Handler) putObjectTagging(c *reqCtx) error {
	var t s3xml.Tagging
	if err := parseXMLBody(c, &t); err != nil {
		return err
	}
	if len(t.TagSet) > 10 {
		return s3err.New(s3err.InvalidTag).WithMessage("Object tags cannot be greater than 10")
	}
	seen := map[string]bool{}
	for _, tag := range t.TagSet {
		if tag.Key == "" || len(tag.Key) > 128 || len(tag.Value) > 256 || seen[tag.Key] {
			return s3err.New(s3err.InvalidTag)
		}
		seen[tag.Key] = true
	}
	vid := c.r.URL.Query().Get("versionId")
	if _, err := h.propose(c, &meta.UpdateObjectMeta{Bucket: c.bucket.Name, Key: c.target.Key, VersionID: vid, Set: map[string]string{metaTagging: encodeTagQuery(t.TagSet)}}); err != nil {
		return err
	}
	if vid != "" {
		versionedHeader(c, vid)
	}
	h.emitTagging(c, "s3:ObjectTagging:Put", vid)
	return c.empty(http.StatusOK)
}

func (h *Handler) deleteObjectTagging(c *reqCtx) error {
	vid := c.r.URL.Query().Get("versionId")
	if _, err := h.propose(c, &meta.UpdateObjectMeta{Bucket: c.bucket.Name, Key: c.target.Key, VersionID: vid, Remove: []string{metaTagging}}); err != nil {
		return err
	}
	h.emitTagging(c, "s3:ObjectTagging:Delete", vid)
	return c.empty(http.StatusNoContent)
}

// emitTagging sends a tagging event for the version whose tags just changed.
// The event carries the object's size and ETag like the other object events;
// a version that vanished in the meantime is reported without them.
func (h *Handler) emitTagging(c *reqCtx, name, versionID string) {
	var size int64
	etag, vid := "", versionID
	if v, err := h.d.DB.GetObject(c.ctx(), c.bucket.Name, c.target.Key, versionID); err == nil {
		size, etag, vid = v.Size, v.ETag, v.VersionID
	}
	h.emit(c, name, c.target.Key, size, etag, vid)
}

func (h *Handler) getObjectRetention(c *reqCtx) error {
	v, err := h.lookupObject(c, c.r.URL.Query().Get("versionId"))
	if err != nil {
		return err
	}
	if v.Metadata[metaLockMode] == "" {
		return &s3err.Error{Code: "NoSuchObjectLockConfiguration", Message: "The specified object does not have a ObjectLock configuration", HTTPStatus: http.StatusNotFound}
	}
	versionedHeader(c, v.VersionID)
	return c.xml(http.StatusOK, &s3xml.Retention{Mode: v.Metadata[metaLockMode], RetainUntilDate: v.Metadata[metaLockUntil]})
}

func (h *Handler) putObjectRetention(c *reqCtx) error {
	if !c.bucket.ObjectLock {
		return s3err.New(s3err.InvalidRequest).WithMessage("Bucket is missing Object Lock Configuration")
	}
	var r s3xml.Retention
	if err := parseXMLBody(c, &r); err != nil {
		return err
	}
	vid := c.r.URL.Query().Get("versionId")
	v, err := h.lookupObject(c, vid)
	if err != nil {
		return err
	}
	cmd := &meta.UpdateObjectMeta{Bucket: c.bucket.Name, Key: c.target.Key, VersionID: v.VersionID}
	if r.Mode == "" && r.RetainUntilDate == "" {
		cmd.Remove = []string{metaLockMode, metaLockUntil}
	} else {
		if r.Mode != "GOVERNANCE" && r.Mode != "COMPLIANCE" {
			return s3err.New(s3err.MalformedXML).WithMessage("Unknown retention mode")
		}
		t, perr := time.Parse(time.RFC3339, r.RetainUntilDate)
		if perr != nil {
			return s3err.New(s3err.MalformedXML).WithMessage("RetainUntilDate must be ISO 8601")
		}
		// Shortening or removing COMPLIANCE retention is not allowed.
		if cur := v.Metadata[metaLockUntil]; cur != "" && v.Metadata[metaLockMode] == "COMPLIANCE" {
			if ct, _ := time.Parse(time.RFC3339, cur); t.Before(ct) || r.Mode != "COMPLIANCE" {
				return s3err.New(s3err.AccessDenied).WithMessage("Cannot weaken COMPLIANCE retention")
			}
		}
		cmd.Set = map[string]string{metaLockMode: r.Mode, metaLockUntil: t.UTC().Format(time.RFC3339)}
	}
	if _, err := h.propose(c, cmd); err != nil {
		return err
	}
	versionedHeader(c, v.VersionID)
	return c.empty(http.StatusOK)
}

func (h *Handler) getObjectLegalHold(c *reqCtx) error {
	v, err := h.lookupObject(c, c.r.URL.Query().Get("versionId"))
	if err != nil {
		return err
	}
	if !c.bucket.ObjectLock {
		return s3err.New(s3err.InvalidRequest).WithMessage("Bucket is missing Object Lock Configuration")
	}
	st := v.Metadata[metaLegalHold]
	if st == "" {
		st = "OFF"
	}
	versionedHeader(c, v.VersionID)
	return c.xml(http.StatusOK, &s3xml.LegalHold{Status: st})
}

func (h *Handler) putObjectLegalHold(c *reqCtx) error {
	if !c.bucket.ObjectLock {
		return s3err.New(s3err.InvalidRequest).WithMessage("Bucket is missing Object Lock Configuration")
	}
	var lh s3xml.LegalHold
	if err := parseXMLBody(c, &lh); err != nil {
		return err
	}
	if lh.Status != "ON" && lh.Status != "OFF" {
		return s3err.New(s3err.MalformedXML).WithMessage("Legal hold status must be ON or OFF")
	}
	vid := c.r.URL.Query().Get("versionId")
	if _, err := h.propose(c, &meta.UpdateObjectMeta{Bucket: c.bucket.Name, Key: c.target.Key, VersionID: vid, Set: map[string]string{metaLegalHold: lh.Status}}); err != nil {
		return err
	}
	return c.empty(http.StatusOK)
}

type objectAttributes struct {
	XMLName      xml.Name            `xml:"GetObjectAttributesOutput"`
	Xmlns        string              `xml:"xmlns,attr,omitempty"`
	ETag         string              `xml:"ETag,omitempty"`
	Checksum     *s3xml.Checksums    `xml:"Checksum,omitempty"`
	ObjectParts  *objectPartsSection `xml:"ObjectParts,omitempty"`
	StorageClass string              `xml:"StorageClass,omitempty"`
	ObjectSize   *int64              `xml:"ObjectSize,omitempty"`
}

type objectPartsSection struct {
	TotalPartsCount      int               `xml:"TotalPartsCount"`
	PartNumberMarker     int               `xml:"PartNumberMarker"`
	NextPartNumberMarker int               `xml:"NextPartNumberMarker"`
	MaxParts             int               `xml:"MaxParts"`
	IsTruncated          bool              `xml:"IsTruncated"`
	Part                 []objectPartEntry `xml:"Part"`
}

type objectPartEntry struct {
	PartNumber int   `xml:"PartNumber"`
	Size       int64 `xml:"Size"`
}

func (h *Handler) getObjectAttributes(c *reqCtx) error {
	v, err := h.lookupObject(c, c.r.URL.Query().Get("versionId"))
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, a := range strings.Split(c.r.Header.Get("x-amz-object-attributes"), ",") {
		want[strings.TrimSpace(a)] = true
	}
	if len(want) == 0 || want[""] {
		return s3err.New(s3err.InvalidRequest).WithMessage("x-amz-object-attributes header is required")
	}
	out := objectAttributes{}
	if want["ETag"] {
		out.ETag = v.ETag
	}
	if want["Checksum"] && checksumAlgoOf(v.Checksums) != "" {
		cks := xmlChecksums(v.Checksums)
		out.Checksum = &cks
	}
	if want["StorageClass"] {
		out.StorageClass = storageClassOf(v.StorageClass)
	}
	if want["ObjectSize"] {
		size := v.Size
		out.ObjectSize = &size
	}
	if want["ObjectParts"] {
		sizes := partSizes(v.Metadata)
		sec := &objectPartsSection{TotalPartsCount: len(sizes), MaxParts: 1000}
		for i, s := range sizes {
			sec.Part = append(sec.Part, objectPartEntry{PartNumber: i + 1, Size: s})
		}
		if len(sizes) > 0 {
			sec.NextPartNumberMarker = len(sizes)
		}
		out.ObjectParts = sec
	}
	c.w.Header().Set("Last-Modified", FormatHTTPTime(v.Created))
	versionedHeader(c, v.VersionID)
	return c.xml(http.StatusOK, &out)
}
