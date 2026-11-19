package worker

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
)

// rule is a parsed, enabled lifecycle rule.
type rule struct {
	prefix       string
	tags         map[string]string
	minSize      *int64
	maxSize      *int64
	expireDays   int
	expireDate   time.Time
	expireMarker bool
	noncurDays   int
	keepNewer    int
	abortDays    int
}

func parseRules(raw string) []rule {
	if raw == "" {
		return nil
	}
	var cfg s3xml.LifecycleConfiguration
	if err := s3xml.Unmarshal(strings.NewReader(raw), &cfg, s3xml.DefaultLimit); err != nil {
		return nil
	}
	var out []rule
	for _, lr := range cfg.Rule {
		if lr.Status != "Enabled" {
			continue
		}
		r := rule{prefix: lr.Prefix}
		if f := lr.Filter; f != nil {
			if f.Prefix != "" {
				r.prefix = f.Prefix
			}
			if f.Tag != nil {
				r.tags = map[string]string{f.Tag.Key: f.Tag.Value}
			}
			r.minSize, r.maxSize = f.ObjectSizeGreaterThan, f.ObjectSizeLessThan
			if a := f.And; a != nil {
				if a.Prefix != "" {
					r.prefix = a.Prefix
				}
				if len(a.Tag) > 0 {
					r.tags = map[string]string{}
					for _, t := range a.Tag {
						r.tags[t.Key] = t.Value
					}
				}
				if a.ObjectSizeGreaterThan != nil {
					r.minSize = a.ObjectSizeGreaterThan
				}
				if a.ObjectSizeLessThan != nil {
					r.maxSize = a.ObjectSizeLessThan
				}
			}
		}
		if e := lr.Expiration; e != nil {
			if e.Days != nil {
				r.expireDays = *e.Days
			}
			if e.Date != "" {
				r.expireDate, _ = s3xml.ParseTime(e.Date)
			}
			if e.ExpiredObjectDeleteMarker != nil {
				r.expireMarker = *e.ExpiredObjectDeleteMarker
			}
		}
		if n := lr.NoncurrentVersionExpiration; n != nil {
			if n.NoncurrentDays != nil {
				r.noncurDays = *n.NoncurrentDays
			}
			if n.NewerNoncurrentVersions != nil {
				r.keepNewer = *n.NewerNoncurrentVersions
			}
		}
		if a := lr.AbortIncompleteMultipartUpload; a != nil && a.DaysAfterInitiation != nil {
			r.abortDays = *a.DaysAfterInitiation
		}
		out = append(out, r)
	}
	return out
}

func (r *rule) matches(v *meta.ObjectVersion) bool {
	if !strings.HasPrefix(v.Key, r.prefix) {
		return false
	}
	if r.minSize != nil && v.Size <= *r.minSize {
		return false
	}
	if r.maxSize != nil && v.Size >= *r.maxSize {
		return false
	}
	if len(r.tags) > 0 {
		vals, err := url.ParseQuery(v.Metadata["x-amz-tagging"])
		if err != nil {
			return false
		}
		for k, want := range r.tags {
			if vals.Get(k) != want {
				return false
			}
		}
	}
	return true
}

// expired reports whether a current version should expire at now.
func (r *rule) expired(v *meta.ObjectVersion, now time.Time) bool {
	if r.expireDays > 0 && !v.Created.Add(time.Duration(r.expireDays)*24*time.Hour).After(now) {
		return true
	}
	if !r.expireDate.IsZero() && !now.Before(r.expireDate) {
		return true
	}
	return false
}

// RunLifecycle applies every bucket's lifecycle rules once.
func (r *Runner) RunLifecycle(ctx context.Context) error {
	buckets, err := r.d.DB.ListBuckets(ctx, "")
	if err != nil {
		return err
	}
	now := r.d.Now()
	for i := range buckets {
		b := &buckets[i]
		rules := parseRules(b.Lifecycle)
		if len(rules) == 0 {
			continue
		}
		if err := r.lifecycleBucket(ctx, b, rules, now); err != nil {
			return err
		}
	}
	r.update(func(s *Stats) { s.LastLifecycle = now })
	return nil
}

func (r *Runner) lifecycleBucket(ctx context.Context, b *meta.Bucket, rules []rule, now time.Time) error {
	// Current versions: expiration.
	cursor := ""
	for {
		res, err := r.d.DB.ListObjects(ctx, b.Name, meta.ListOpts{StartAfter: cursor, Max: 1000})
		if err != nil {
			return err
		}
		for i := range res.Objects {
			v := &res.Objects[i]
			for _, rl := range rules {
				if !rl.matches(v) || !rl.expired(v, now) {
					continue
				}
				if v.Metadata["x-amz-object-lock-legal-hold"] == "ON" {
					break
				}
				if until := v.Metadata["x-amz-object-lock-retain-until-date"]; until != "" {
					if t, perr := time.Parse(time.RFC3339, until); perr == nil && t.After(now) {
						break
					}
				}
				if err := r.propose(ctx, &meta.DeleteObject{Bucket: b.Name, Key: v.Key, MarkerVersionID: ids.VersionID(), Now: now.UnixMilli()}); err != nil {
					return err
				}
				r.update(func(s *Stats) { s.ObjectsExpired++ })
				break
			}
		}
		if !res.IsTruncated {
			break
		}
		cursor = res.NextCursor
	}

	// Noncurrent versions and expired delete markers.
	needVersions := false
	for _, rl := range rules {
		if rl.noncurDays > 0 || rl.expireMarker {
			needVersions = true
		}
	}
	if needVersions && b.Versioning != meta.VersioningOff {
		if err := r.lifecycleVersions(ctx, b, rules, now); err != nil {
			return err
		}
	}

	// Incomplete multipart uploads.
	for _, rl := range rules {
		if rl.abortDays <= 0 {
			continue
		}
		cutoff := now.Add(-time.Duration(rl.abortDays) * 24 * time.Hour)
		res, err := r.d.DB.ListUploads(ctx, b.Name, meta.UploadListOpts{Prefix: rl.prefix, Max: 1000})
		if err != nil {
			return err
		}
		for _, u := range res.Uploads {
			if u.Created.After(cutoff) {
				continue
			}
			if err := r.propose(ctx, &meta.AbortUpload{UploadID: u.ID, Now: now.UnixMilli()}); err != nil {
				return err
			}
			r.update(func(s *Stats) { s.UploadsAborted++ })
		}
	}
	return nil
}

func (r *Runner) lifecycleVersions(ctx context.Context, b *meta.Bucket, rules []rule, now time.Time) error {
	opts := meta.VersionListOpts{Max: 1000}
	// Group versions by key across pages; a key's versions are contiguous.
	var pending []meta.ObjectVersion
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		err := r.lifecycleKey(ctx, b, rules, pending, now)
		pending = pending[:0]
		return err
	}
	for {
		res, err := r.d.DB.ListVersions(ctx, b.Name, opts)
		if err != nil {
			return err
		}
		for _, v := range res.Versions {
			if len(pending) > 0 && pending[0].Key != v.Key {
				if err := flush(); err != nil {
					return err
				}
			}
			pending = append(pending, v)
		}
		if !res.IsTruncated {
			break
		}
		opts.KeyMarker, opts.VersionIDMarker = res.NextKeyMarker, res.NextVersionIDMarker
	}
	return flush()
}

// lifecycleKey handles one key's versions, newest first.
func (r *Runner) lifecycleKey(ctx context.Context, b *meta.Bucket, rules []rule, versions []meta.ObjectVersion, now time.Time) error {
	latest := &versions[0]
	// Expired object delete marker: latest is a marker and nothing else remains.
	if latest.DeleteMarker && len(versions) == 1 {
		for _, rl := range rules {
			if rl.expireMarker && strings.HasPrefix(latest.Key, rl.prefix) {
				if err := r.propose(ctx, &meta.DeleteObject{Bucket: b.Name, Key: latest.Key, VersionID: latest.VersionID, Now: now.UnixMilli()}); err != nil {
					return err
				}
				r.update(func(s *Stats) { s.MarkersRemoved++ })
				break
			}
		}
		return nil
	}
	// Noncurrent versions: the i-th noncurrent version became noncurrent when
	// versions[i-1] was created.
	for _, rl := range rules {
		if rl.noncurDays <= 0 {
			continue
		}
		noncurrent := 0
		for i := 1; i < len(versions); i++ {
			v := &versions[i]
			if !rl.matches(v) {
				continue
			}
			noncurrent++
			if noncurrent <= rl.keepNewer {
				continue
			}
			since := versions[i-1].Created
			if since.Add(time.Duration(rl.noncurDays) * 24 * time.Hour).After(now) {
				continue
			}
			if v.Metadata["x-amz-object-lock-legal-hold"] == "ON" {
				continue
			}
			if until := v.Metadata["x-amz-object-lock-retain-until-date"]; until != "" {
				if t, perr := time.Parse(time.RFC3339, until); perr == nil && t.After(now) {
					continue
				}
			}
			if err := r.propose(ctx, &meta.DeleteObject{Bucket: b.Name, Key: v.Key, VersionID: v.VersionID, Now: now.UnixMilli()}); err != nil {
				return err
			}
			r.update(func(s *Stats) { s.VersionsExpired++ })
		}
	}
	return nil
}
