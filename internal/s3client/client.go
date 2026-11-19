// Package s3client is the minimal signing S3 client the server uses to talk
// to other S3 endpoints: standby servers for replication and sources or
// destinations for transfers.
package s3client

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/enetforge/hogshead/internal/auth"
)

// Endpoint identifies a bucket on a remote S3 service and how to sign for it.
type Endpoint struct {
	Endpoint  string `json:"endpoint"` // e.g. https://backup.example.com:9000
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"` //nolint:gosec // remote credentials by design
	SecretKey string `json:"secret_key"` //nolint:gosec
	Region    string `json:"region,omitempty"`
	PathStyle bool   `json:"path_style"`
}

// Validate checks the endpoint URL and credentials; bucket is required when needBucket.
func (e Endpoint) Validate(needBucket bool) error {
	if !strings.HasPrefix(e.Endpoint, "http://") && !strings.HasPrefix(e.Endpoint, "https://") {
		return errors.New("endpoint must start with http:// or https://")
	}
	if u, err := url.Parse(e.Endpoint); err != nil || u.Host == "" {
		return fmt.Errorf("invalid endpoint %q", e.Endpoint)
	}
	if e.AccessKey == "" || e.SecretKey == "" {
		return errors.New("access_key and secret_key are required")
	}
	if needBucket && e.Bucket == "" {
		return errors.New("bucket is required")
	}
	return nil
}

// Error is a non-2xx response from the remote.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("remote returned %d: %s", e.Status, e.Body) }

// ErrNotFound is returned by HeadObject and GetObject for a missing key.
var ErrNotFound = errors.New("object not found")

// OriginHeader names the server a replicated change came from. A hogshead
// server receiving it records the origin on its log so the change is not
// forwarded again; other S3 services ignore the header.
const OriginHeader = "X-Hogshead-Origin"

// SourceVersionHeader and SourceTimeHeader carry the version id and creation
// time (unix milliseconds) a replicated put or delete marker had at its
// origin. A hogshead receiver reuses them, which makes replication
// idempotent (a version it already holds is a no-op) and lets "latest"
// follow the original write time instead of arrival order.
const (
	SourceVersionHeader = "X-Hogshead-Source-Version"
	SourceTimeHeader    = "X-Hogshead-Source-Time"
)

// Client signs requests for one Endpoint.
type Client struct {
	e      Endpoint
	http   *http.Client
	origin string
}

// New builds a client with a generous timeout suited to large objects.
func New(e Endpoint) *Client {
	return &Client{e: e, http: &http.Client{Timeout: 10 * time.Minute}}
}

// WithOrigin returns a client that tags every request with this server's id.
func (c *Client) WithOrigin(id string) *Client {
	cp := *c
	cp.origin = id
	return &cp
}

// BucketOptions describe how EnsureBucket creates a missing bucket.
type BucketOptions struct {
	Versioned  bool
	ObjectLock bool // implies versioning
}

func (c *Client) region() string {
	if c.e.Region == "" {
		return "us-east-1"
	}
	return c.e.Region
}

// ObjectURL builds the URL for a key, path-style or virtual-host style.
func (c *Client) ObjectURL(key string) (string, error) {
	u, err := url.Parse(strings.TrimRight(c.e.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid endpoint %q", c.e.Endpoint)
	}
	base := ""
	if c.e.PathStyle {
		base = "/" + c.e.Bucket
	} else {
		u.Host = c.e.Bucket + "." + u.Host
	}
	u.Path, u.RawPath = base, base
	if key != "" {
		u.Path += "/" + key
		u.RawPath += "/" + EscapeKey(key)
	}
	return u.String(), nil
}

// EscapeKey percent-encodes each path segment of a key.
func EscapeKey(k string) string {
	parts := strings.Split(k, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (c *Client) do(ctx context.Context, method, key, query string, body io.Reader, size int64, headers map[string]string) (*http.Response, error) {
	u, err := c.ObjectURL(key)
	if err != nil {
		return nil, err
	}
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if c.origin != "" {
		req.Header.Set(OriginHeader, c.origin)
	}
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	if err := auth.Sign(req, auth.Credentials{AccessKeyID: c.e.AccessKey, SecretKey: c.e.SecretKey}, c.region(), "s3", time.Now(), ""); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return resp, nil
}

func isStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// PutObject uploads an object with its headers.
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, size int64, headers map[string]string) error {
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Content-Length"] = strconv.FormatInt(size, 10)
	resp, err := c.do(ctx, http.MethodPut, key, "", body, size, headers)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// DeleteObject deletes the current version; a missing key is not an error.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	return c.DeleteObjectVersion(ctx, key, "", nil)
}

// DeleteObjectVersion deletes one version (or the current one when versionID
// is empty) with extra headers; a missing key or version is not an error.
func (c *Client) DeleteObjectVersion(ctx context.Context, key, versionID string, headers map[string]string) error {
	query := ""
	if versionID != "" {
		query = "versionId=" + url.QueryEscape(versionID)
	}
	resp, err := c.do(ctx, http.MethodDelete, key, query, nil, 0, headers)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return resp.Body.Close()
}

// EnsureBucket checks the bucket exists and creates it when the remote
// reports 404, with Object Lock when asked (which implies versioning) and
// otherwise with versioning when asked. It reports whether it created it.
func (c *Client) EnsureBucket(ctx context.Context, o BucketOptions) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, "", "", nil, 0, nil)
	if err == nil {
		return false, resp.Body.Close()
	}
	if !isStatus(err, http.StatusNotFound) {
		return false, err
	}
	var headers map[string]string
	if o.ObjectLock {
		headers = map[string]string{"x-amz-bucket-object-lock-enabled": "true"}
	}
	resp, err = c.do(ctx, http.MethodPut, "", "", nil, 0, headers)
	if err != nil {
		return false, fmt.Errorf("create bucket %s: %w", c.e.Bucket, err)
	}
	_ = resp.Body.Close()
	if !o.Versioned && !o.ObjectLock {
		return true, nil
	}
	if err := c.PutBucketConfig(ctx, "versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`); err != nil {
		return true, fmt.Errorf("enable versioning on %s: %w", c.e.Bucket, err)
	}
	return true, nil
}

// PutBucketConfig writes a bucket sub-resource (versioning, policy, cors,
// lifecycle, tagging, object-lock, notification) from its raw document.
func (c *Client) PutBucketConfig(ctx context.Context, subresource, body string) error {
	ct := "application/xml"
	if subresource == "policy" {
		ct = "application/json"
	}
	resp, err := c.do(ctx, http.MethodPut, "", subresource, strings.NewReader(body), int64(len(body)), map[string]string{"content-type": ct})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// DeleteBucketConfig removes a bucket sub-resource; a missing one is not an error.
func (c *Client) DeleteBucketConfig(ctx context.Context, subresource string) error {
	resp, err := c.do(ctx, http.MethodDelete, "", subresource, nil, 0, nil)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return resp.Body.Close()
}

// AdminJSON calls the hogshead admin API of the remote (which must be a
// hogshead server) with the client's credentials. out may be nil.
func (c *Client) AdminJSON(ctx context.Context, method, path string, in, out any) error {
	u, err := url.Parse(strings.TrimRight(c.e.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid endpoint %q", c.e.Endpoint)
	}
	if i := strings.Index(path, "?"); i >= 0 {
		u.RawQuery = path[i+1:]
		path = path[:i]
	}
	u.Path, u.RawPath = "/_admin/v1"+path, ""
	var body io.Reader
	var raw []byte
	if in != nil {
		if raw, err = json.Marshal(in); err != nil {
			return err
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.ContentLength = int64(len(raw))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.origin != "" {
		req.Header.Set(OriginHeader, c.origin)
	}
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	if err := auth.Sign(req, auth.Credentials{AccessKeyID: c.e.AccessKey, SecretKey: c.e.SecretKey}, c.region(), "s3", time.Now(), ""); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// PutObjectSubresource writes an object sub-resource (retention, legal-hold,
// tagging) of one version from its raw XML document.
func (c *Client) PutObjectSubresource(ctx context.Context, key, versionID, subresource, body string, headers map[string]string) error {
	query := subresource
	if versionID != "" {
		query += "&versionId=" + url.QueryEscape(versionID)
	}
	h := map[string]string{"content-type": "application/xml"}
	for k, v := range headers {
		h[k] = v
	}
	resp, err := c.do(ctx, http.MethodPut, key, query, strings.NewReader(body), int64(len(body)), h)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// DeleteObjectSubresource removes an object sub-resource (tagging) of one
// version; a missing object is not an error.
func (c *Client) DeleteObjectSubresource(ctx context.Context, key, versionID, subresource string) error {
	query := subresource
	if versionID != "" {
		query += "&versionId=" + url.QueryEscape(versionID)
	}
	resp, err := c.do(ctx, http.MethodDelete, key, query, nil, 0, nil)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return resp.Body.Close()
}

// ObjectInfo describes a remote object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string // without quotes
	VersionID    string // x-amz-version-id of a HEAD or GET; empty for listings
	LastModified time.Time
	ContentType  string
	// Headers holds the response headers of a HEAD or GET (lower-case keys);
	// empty for listing entries.
	Headers map[string]string
}

// Object is a GET response: info plus the body, which the caller closes.
type Object struct {
	ObjectInfo
	Body io.ReadCloser
}

func infoFromHeaders(key string, h http.Header) ObjectInfo {
	info := ObjectInfo{Key: key, ETag: strings.Trim(h.Get("ETag"), `"`), VersionID: h.Get("X-Amz-Version-Id"), ContentType: h.Get("Content-Type"), Headers: map[string]string{}}
	info.Size, _ = strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	if t, err := http.ParseTime(h.Get("Last-Modified")); err == nil {
		info.LastModified = t
	}
	for k, v := range h {
		if len(v) > 0 {
			info.Headers[strings.ToLower(k)] = v[0]
		}
	}
	return info
}

// HeadObject returns an object's metadata or ErrNotFound.
func (c *Client) HeadObject(ctx context.Context, key string) (*ObjectInfo, error) {
	return c.HeadObjectVersion(ctx, key, "")
}

// HeadObjectVersion returns one version's metadata or ErrNotFound.
func (c *Client) HeadObjectVersion(ctx context.Context, key, versionID string) (*ObjectInfo, error) {
	query := ""
	if versionID != "" {
		query = "versionId=" + url.QueryEscape(versionID)
	}
	resp, err := c.do(ctx, http.MethodHead, key, query, nil, 0, nil)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = resp.Body.Close()
	info := infoFromHeaders(key, resp.Header)
	return &info, nil
}

// GetObject streams an object or returns ErrNotFound. The caller closes Body.
func (c *Client) GetObject(ctx context.Context, key string) (*Object, error) {
	resp, err := c.do(ctx, http.MethodGet, key, "", nil, 0, nil) //nolint:bodyclose // the body is returned to the caller
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &Object{ObjectInfo: infoFromHeaders(key, resp.Header), Body: resp.Body}, nil
}

// VersionInfo is one entry of ListObjectVersions.
type VersionInfo struct {
	Key          string
	VersionID    string
	IsLatest     bool
	DeleteMarker bool
}

type versionsXML struct {
	IsTruncated         bool   `xml:"IsTruncated"`
	NextKeyMarker       string `xml:"NextKeyMarker"`
	NextVersionIDMarker string `xml:"NextVersionIdMarker"`
	Versions            []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
		IsLatest  bool   `xml:"IsLatest"`
	} `xml:"Version"`
	Markers []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
		IsLatest  bool   `xml:"IsLatest"`
	} `xml:"DeleteMarker"`
}

// ListVersions returns one page of ListObjectVersions; pass the returned
// markers back to continue.
func (c *Client) ListVersions(ctx context.Context, keyMarker, versionMarker string) ([]VersionInfo, bool, string, string, error) {
	q := url.Values{"versions": {""}, "max-keys": {"1000"}}
	if keyMarker != "" {
		q.Set("key-marker", keyMarker)
	}
	if versionMarker != "" {
		q.Set("version-id-marker", versionMarker)
	}
	resp, err := c.do(ctx, http.MethodGet, "", q.Encode(), nil, 0, nil)
	if err != nil {
		return nil, false, "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var x versionsXML
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&x); err != nil {
		return nil, false, "", "", fmt.Errorf("parse versions: %w", err)
	}
	var out []VersionInfo
	for _, v := range x.Versions {
		out = append(out, VersionInfo{Key: v.Key, VersionID: v.VersionID, IsLatest: v.IsLatest})
	}
	for _, m := range x.Markers {
		out = append(out, VersionInfo{Key: m.Key, VersionID: m.VersionID, IsLatest: m.IsLatest, DeleteMarker: true})
	}
	return out, x.IsTruncated, x.NextKeyMarker, x.NextVersionIDMarker, nil
}

// PurgeBucket deletes every version and marker, then the bucket itself. A
// missing bucket is not an error.
func (c *Client) PurgeBucket(ctx context.Context) error {
	for {
		vs, truncated, km, vm, err := c.ListVersions(ctx, "", "")
		if err != nil {
			if isStatus(err, http.StatusNotFound) {
				return nil
			}
			return err
		}
		for _, v := range vs {
			if err := c.DeleteObjectVersion(ctx, v.Key, v.VersionID, map[string]string{"x-amz-bypass-governance-retention": "true"}); err != nil {
				return err
			}
		}
		if !truncated || (km == "" && vm == "") {
			break
		}
		_ = km
	}
	resp, err := c.do(ctx, http.MethodDelete, "", "", nil, 0, nil)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	return resp.Body.Close()
}

// Listing is one page of ListObjectsV2.
type Listing struct {
	Objects   []ObjectInfo
	Truncated bool
	NextToken string
}

type listXML struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

// ListObjects returns one page of keys under prefix; pass the previous
// page's NextToken to continue.
func (c *Client) ListObjects(ctx context.Context, prefix, token string) (*Listing, error) {
	q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	resp, err := c.do(ctx, http.MethodGet, "", q.Encode(), nil, 0, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var x listXML
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&x); err != nil {
		return nil, fmt.Errorf("parse listing: %w", err)
	}
	out := &Listing{Truncated: x.IsTruncated, NextToken: x.NextContinuationToken}
	for _, e := range x.Contents {
		info := ObjectInfo{Key: e.Key, Size: e.Size, ETag: strings.Trim(e.ETag, `"`)}
		info.LastModified, _ = time.Parse(time.RFC3339Nano, e.LastModified)
		out.Objects = append(out.Objects, info)
	}
	return out, nil
}
