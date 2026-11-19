package api

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strings"

	"github.com/enetforge/hogshead/internal/api/s3err"
	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/policy"
)

func (h *Handler) listBuckets(c *reqCtx) error {
	all, err := h.d.DB.ListBuckets(c.ctx(), "")
	if err != nil {
		return err
	}
	out := s3xml.ListAllMyBucketsResult{Owner: *h.owner(c, h.callerID(c))}
	for _, b := range all {
		if policy.VisibleBucket(c.caller, b.Name) {
			out.Buckets = append(out.Buckets, s3xml.Bucket{Name: b.Name, CreationDate: s3xml.FormatTime(b.CreatedAt), BucketRegion: h.d.Region})
		}
	}
	if out.Buckets == nil {
		out.Buckets = []s3xml.Bucket{}
	}
	return c.xml(http.StatusOK, &out)
}

func (h *Handler) createBucket(c *reqCtx) error {
	if !ValidBucketName(c.target.Bucket) {
		return s3err.New(s3err.InvalidBucketName)
	}
	if c.r.ContentLength != 0 {
		var cfg s3xml.CreateBucketConfiguration
		if err := parseXMLBody(c, &cfg); err != nil && c.r.ContentLength > 0 {
			return err
		}
		if cfg.LocationConstraint != "" && cfg.LocationConstraint != h.d.Region && h.d.Region != "us-east-1" {
			return s3err.New(s3err.InvalidLocationConstraint)
		}
	}
	lock := strings.EqualFold(c.r.Header.Get("x-amz-bucket-object-lock-enabled"), "true")
	if _, err := h.propose(c, &meta.CreateBucket{Name: c.target.Bucket, Owner: h.callerID(c), Now: h.nowMs(), ObjectLock: lock}); err != nil {
		var me *meta.Error
		if errors.As(err, &me) && me.Code == meta.CodeBucketAlreadyOwned && h.d.Region == "us-east-1" {
			// Legacy us-east-1 behaviour: re-creating your own bucket succeeds.
			c.w.Header().Set("Location", "/"+c.target.Bucket)
			return c.empty(http.StatusOK)
		}
		return err
	}
	if doc := cannedBucketPolicy(c.r.Header, c.target.Bucket); doc != "" {
		if _, err := h.propose(c, &meta.SetBucketPolicy{Name: c.target.Bucket, Policy: doc}); err != nil {
			return err
		}
	}
	c.w.Header().Set("Location", "/"+c.target.Bucket)
	return c.empty(http.StatusOK)
}

// cannedBucketPolicy maps canned/grant headers to a policy document; "" for
// private or unknown.
func cannedBucketPolicy(h http.Header, bucket string) string {
	switch h.Get("x-amz-acl") {
	case "public-read":
		return policy.PublicReadPolicy(bucket)
	case "public-read-write":
		return policy.PublicReadWritePolicy(bucket)
	}
	read := strings.Contains(h.Get("x-amz-grant-read"), "AllUsers") || strings.Contains(h.Get("x-amz-grant-full-control"), "AllUsers")
	write := strings.Contains(h.Get("x-amz-grant-write"), "AllUsers") || strings.Contains(h.Get("x-amz-grant-full-control"), "AllUsers")
	switch {
	case read && write:
		return policy.PublicReadWritePolicy(bucket)
	case read:
		return policy.PublicReadPolicy(bucket)
	}
	return ""
}

func (h *Handler) deleteBucket(c *reqCtx) error {
	if _, err := h.propose(c, &meta.DeleteBucket{Name: c.target.Bucket, Now: h.nowMs()}); err != nil {
		return err
	}
	return c.empty(http.StatusNoContent)
}

func (h *Handler) headBucket(c *reqCtx) error {
	c.w.Header().Set("x-amz-bucket-region", h.d.Region)
	return c.empty(http.StatusOK)
}

func (h *Handler) getBucketLocation(c *reqCtx) error {
	loc := h.d.Region
	if loc == "us-east-1" {
		loc = ""
	}
	return c.xml(http.StatusOK, &s3xml.LocationConstraint{Location: loc})
}

func (h *Handler) getBucketVersioning(c *reqCtx) error {
	out := s3xml.VersioningConfiguration{Status: c.bucket.Versioning}
	return c.xml(http.StatusOK, &out)
}

func (h *Handler) putBucketVersioning(c *reqCtx) error {
	var cfg s3xml.VersioningConfiguration
	if err := parseXMLBody(c, &cfg); err != nil {
		return err
	}
	if cfg.Status != meta.VersioningEnabled && cfg.Status != meta.VersioningSuspended {
		return s3err.New(s3err.IllegalVersioningConfigurationException)
	}
	if _, err := h.propose(c, &meta.SetBucketVersioning{Name: c.target.Bucket, State: cfg.Status}); err != nil {
		return err
	}
	return c.empty(http.StatusOK)
}

func (h *Handler) getBucketPolicy(c *reqCtx) error {
	if c.bucket.Policy == "" {
		return s3err.New(s3err.NoSuchBucketPolicy)
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write([]byte(c.bucket.Policy))
	return nil
}

func (h *Handler) putBucketPolicy(c *reqCtx) error {
	body, err := readBodyLimited(c, policy.MaxSize)
	if err != nil {
		return err
	}
	if _, err := policy.Parse(body, c.target.Bucket); err != nil {
		return s3err.Newf(s3err.MalformedPolicy, "%v", err)
	}
	if _, err := h.propose(c, &meta.SetBucketPolicy{Name: c.target.Bucket, Policy: string(body)}); err != nil {
		return err
	}
	return c.empty(http.StatusNoContent)
}

func (h *Handler) deleteBucketPolicy(c *reqCtx) error {
	if _, err := h.propose(c, &meta.SetBucketPolicy{Name: c.target.Bucket}); err != nil {
		return err
	}
	return c.empty(http.StatusNoContent)
}

type policyStatus struct {
	XMLName  xml.Name `xml:"PolicyStatus"`
	Xmlns    string   `xml:"xmlns,attr,omitempty"`
	IsPublic bool     `xml:"IsPublic"`
}

func (h *Handler) getBucketPolicyStatus(c *reqCtx) error {
	if c.bucket.Policy == "" {
		return s3err.New(s3err.NoSuchBucketPolicy)
	}
	return c.xml(http.StatusOK, &policyStatus{IsPublic: c.pol.IsPublicRead(c.bucket.Name)})
}

func (h *Handler) getBucketCors(c *reqCtx) error {
	cfg := parseCORS(c.bucket.CORS)
	if cfg == nil {
		return s3err.New(s3err.NoSuchCORSConfiguration)
	}
	return c.xml(http.StatusOK, cfg)
}

func (h *Handler) putBucketCors(c *reqCtx) error {
	var cfg s3xml.CORSConfiguration
	if err := parseXMLBody(c, &cfg); err != nil {
		return err
	}
	if err := ValidateCORS(&cfg); err != nil {
		return err
	}
	raw, err := s3xml.Marshal(&cfg)
	if err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.SetBucketCORS{Name: c.target.Bucket, CORS: string(raw)}); err != nil {
		return err
	}
	return c.empty(http.StatusOK)
}

func (h *Handler) deleteBucketCors(c *reqCtx) error {
	if _, err := h.propose(c, &meta.SetBucketCORS{Name: c.target.Bucket}); err != nil {
		return err
	}
	return c.empty(http.StatusNoContent)
}

func (h *Handler) getBucketLifecycle(c *reqCtx) error {
	if c.bucket.Lifecycle == "" {
		return s3err.New(s3err.NoSuchLifecycleConfiguration)
	}
	var cfg s3xml.LifecycleConfiguration
	if err := s3xml.Unmarshal(strings.NewReader(c.bucket.Lifecycle), &cfg, s3xml.DefaultLimit); err != nil {
		return err
	}
	return c.xml(http.StatusOK, &cfg)
}

func ValidateLifecycle(cfg *s3xml.LifecycleConfiguration) error {
	if len(cfg.Rule) == 0 {
		return s3err.New(s3err.MalformedXML).WithMessage("At least one Rule is required")
	}
	if len(cfg.Rule) > 1000 {
		return s3err.New(s3err.InvalidRequest).WithMessage("At most 1000 lifecycle rules are allowed")
	}
	seen := map[string]bool{}
	for i := range cfg.Rule {
		r := &cfg.Rule[i]
		if r.ID != "" {
			if seen[r.ID] {
				return s3err.New(s3err.InvalidArgument).WithMessage("Rule ID must be unique")
			}
			seen[r.ID] = true
		}
		if r.Status != "Enabled" && r.Status != "Disabled" {
			return s3err.New(s3err.MalformedXML).WithMessage("Rule Status must be Enabled or Disabled")
		}
		if r.Expiration == nil && r.NoncurrentVersionExpiration == nil && r.AbortIncompleteMultipartUpload == nil {
			return s3err.New(s3err.InvalidRequest).WithMessage("Rule needs Expiration, NoncurrentVersionExpiration or AbortIncompleteMultipartUpload")
		}
		if e := r.Expiration; e != nil {
			n := 0
			if e.Days != nil {
				if *e.Days <= 0 {
					return s3err.New(s3err.InvalidArgument).WithMessage("Expiration Days must be positive")
				}
				n++
			}
			if e.Date != "" {
				if _, err := s3xml.ParseTime(e.Date); err != nil {
					return s3err.New(s3err.InvalidArgument).WithMessage("Expiration Date must be ISO 8601")
				}
				n++
			}
			if e.ExpiredObjectDeleteMarker != nil && *e.ExpiredObjectDeleteMarker {
				n++
			}
			if n != 1 {
				return s3err.New(s3err.MalformedXML).WithMessage("Expiration needs exactly one of Days, Date or ExpiredObjectDeleteMarker")
			}
		}
		if n := r.NoncurrentVersionExpiration; n != nil && (n.NoncurrentDays == nil || *n.NoncurrentDays <= 0) {
			return s3err.New(s3err.InvalidArgument).WithMessage("NoncurrentDays must be positive")
		}
		if a := r.AbortIncompleteMultipartUpload; a != nil && (a.DaysAfterInitiation == nil || *a.DaysAfterInitiation <= 0) {
			return s3err.New(s3err.InvalidArgument).WithMessage("DaysAfterInitiation must be positive")
		}
	}
	return nil
}

func (h *Handler) putBucketLifecycle(c *reqCtx) error {
	var cfg s3xml.LifecycleConfiguration
	if err := parseXMLBody(c, &cfg); err != nil {
		return err
	}
	if err := ValidateLifecycle(&cfg); err != nil {
		return err
	}
	raw, err := s3xml.Marshal(&cfg)
	if err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.SetBucketLifecycle{Name: c.target.Bucket, Lifecycle: string(raw)}); err != nil {
		return err
	}
	return c.empty(http.StatusOK)
}

func (h *Handler) deleteBucketLifecycle(c *reqCtx) error {
	if _, err := h.propose(c, &meta.SetBucketLifecycle{Name: c.target.Bucket}); err != nil {
		return err
	}
	return c.empty(http.StatusNoContent)
}

func (h *Handler) getBucketAcl(c *reqCtx) error {
	canned := ""
	switch {
	case c.pol.IsPublicWrite(c.bucket.Name):
		canned = "public-read-write"
	case c.pol.IsPublicRead(c.bucket.Name):
		canned = "public-read"
	}
	acl := h.aclDocument(c, c.bucket.Owner, canned)
	return c.xml(http.StatusOK, &acl)
}

// putBucketAcl maps canned ACLs, grant headers and AllUsers grants in an
// XML body onto the equivalent bucket policy. Private removes a canned
// public policy; a hand-written policy is left alone.
func (h *Handler) putBucketAcl(c *reqCtx) error {
	doc := cannedBucketPolicy(c.r.Header, c.target.Bucket)
	explicitPrivate := c.r.Header.Get("x-amz-acl") == "private"
	if doc == "" && c.r.Header.Get("x-amz-acl") == "" && c.r.ContentLength != 0 {
		canned, err := aclFromBody(c)
		if err != nil {
			return err
		}
		switch canned {
		case "public-read":
			doc = policy.PublicReadPolicy(c.target.Bucket)
		case "public-read-write":
			doc = policy.PublicReadWritePolicy(c.target.Bucket)
		default:
			explicitPrivate = true
		}
	}
	switch {
	case doc != "":
		if _, err := h.propose(c, &meta.SetBucketPolicy{Name: c.target.Bucket, Policy: doc}); err != nil {
			return err
		}
	case explicitPrivate && policy.IsCannedPublic(c.bucket.Policy, c.target.Bucket):
		if _, err := h.propose(c, &meta.SetBucketPolicy{Name: c.target.Bucket}); err != nil {
			return err
		}
	}
	return c.empty(http.StatusOK)
}

func bucketTaggingKey(bucket string) string { return "bucket.tagging:" + bucket }

func (h *Handler) getBucketTagging(c *reqCtx) error {
	raw, err := h.d.DB.GetSetting(c.ctx(), bucketTaggingKey(c.target.Bucket))
	if err != nil {
		return err
	}
	if raw == "" {
		return s3err.New(s3err.NoSuchTagSet)
	}
	var t s3xml.Tagging
	if err := s3xml.Unmarshal(strings.NewReader(raw), &t, s3xml.DefaultLimit); err != nil {
		return err
	}
	return c.xml(http.StatusOK, &t)
}

func (h *Handler) putBucketTagging(c *reqCtx) error {
	var t s3xml.Tagging
	if err := parseXMLBody(c, &t); err != nil {
		return err
	}
	if len(t.TagSet) > 50 {
		return s3err.New(s3err.InvalidRequest).WithMessage("At most 50 tags are allowed")
	}
	raw, err := s3xml.Marshal(&t)
	if err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.SetSetting{Key: bucketTaggingKey(c.target.Bucket), Value: string(raw)}); err != nil {
		return err
	}
	return c.empty(http.StatusNoContent)
}

func (h *Handler) deleteBucketTagging(c *reqCtx) error {
	if _, err := h.propose(c, &meta.SetSetting{Key: bucketTaggingKey(c.target.Bucket)}); err != nil {
		return err
	}
	return c.empty(http.StatusNoContent)
}

func (h *Handler) getBucketEncryption(c *reqCtx) error {
	// Encryption at rest is a node setting, not per bucket. Report AES256
	// when the master key is configured so clients see SSE-S3 semantics.
	if !h.encryptedAtRest() {
		return &s3err.Error{Code: "ServerSideEncryptionConfigurationNotFoundError", Message: "The server side encryption configuration was not found", HTTPStatus: http.StatusNotFound}
	}
	out := s3xml.ServerSideEncryptionConfiguration{Rule: []s3xml.ServerSideEncryptionRule{{
		ApplyServerSideEncryptionByDefault: &s3xml.ApplyServerSideEncryptionByDefault{SSEAlgorithm: "AES256"},
	}}}
	return c.xml(http.StatusOK, &out)
}

func (h *Handler) encryptedAtRest() bool {
	type keyed interface{ Encrypted() bool }
	if k, ok := h.d.Chunks.(keyed); ok {
		return k.Encrypted()
	}
	return false
}

func lockConfigKey(bucket string) string { return "bucket.lock:" + bucket }

// defaultRetention returns the bucket's default retention rule, if any.
func (h *Handler) defaultRetention(c *reqCtx) *s3xml.DefaultRetention {
	raw, err := h.d.DB.GetSetting(c.ctx(), lockConfigKey(c.bucket.Name))
	if err != nil || raw == "" {
		return nil
	}
	var cfg s3xml.ObjectLockConfiguration
	if s3xml.Unmarshal(strings.NewReader(raw), &cfg, s3xml.DefaultLimit) != nil || cfg.Rule == nil {
		return nil
	}
	return &cfg.Rule.DefaultRetention
}

func (h *Handler) getObjectLockConfig(c *reqCtx) error {
	if !c.bucket.ObjectLock {
		return s3err.New(s3err.ObjectLockConfigurationNotFoundError)
	}
	raw, _ := h.d.DB.GetSetting(c.ctx(), lockConfigKey(c.bucket.Name))
	if raw != "" {
		var cfg s3xml.ObjectLockConfiguration
		if err := s3xml.Unmarshal(strings.NewReader(raw), &cfg, s3xml.DefaultLimit); err == nil {
			return c.xml(http.StatusOK, &cfg)
		}
	}
	return c.xml(http.StatusOK, &s3xml.ObjectLockConfiguration{ObjectLockEnabled: "Enabled"})
}

func (h *Handler) putObjectLockConfig(c *reqCtx) error {
	if !c.bucket.ObjectLock {
		return s3err.New(s3err.InvalidBucketState).WithMessage("Object Lock configuration cannot be enabled on existing buckets")
	}
	var cfg s3xml.ObjectLockConfiguration
	if err := parseXMLBody(c, &cfg); err != nil {
		return err
	}
	if cfg.ObjectLockEnabled != "Enabled" {
		return s3err.New(s3err.MalformedXML).WithMessage("ObjectLockEnabled must be Enabled")
	}
	raw := ""
	if cfg.Rule != nil {
		dr := cfg.Rule.DefaultRetention
		if dr.Mode != "GOVERNANCE" && dr.Mode != "COMPLIANCE" {
			return s3err.New(s3err.MalformedXML).WithMessage("DefaultRetention Mode must be GOVERNANCE or COMPLIANCE")
		}
		days, years := 0, 0
		if dr.Days != nil {
			days = *dr.Days
		}
		if dr.Years != nil {
			years = *dr.Years
		}
		if (days > 0) == (years > 0) {
			return s3err.New(s3err.MalformedXML).WithMessage("DefaultRetention needs exactly one of Days or Years")
		}
		b, err := s3xml.Marshal(&cfg)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	if _, err := h.propose(c, &meta.SetSetting{Key: lockConfigKey(c.bucket.Name), Value: raw}); err != nil {
		return err
	}
	return c.empty(http.StatusOK)
}
