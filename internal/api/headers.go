package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxUserMetadataBytes is the AWS limit on user metadata size.
const MaxUserMetadataBytes = 2048

// systemHeaders are stored with the object and echoed on GET/HEAD.
var systemHeaders = []string{
	"Content-Type", "Content-Encoding", "Content-Language", "Content-Disposition",
	"Cache-Control", "Expires", "X-Amz-Website-Redirect-Location", "X-Amz-Storage-Class",
}

// ErrMetadataTooLarge is returned when user metadata exceeds the limit.
var ErrMetadataTooLarge = errors.New("metadata too large")

// ExtractMetadata collects user (x-amz-meta-*) and stored system headers from
// a request into the map persisted with the object version.
func ExtractMetadata(h http.Header) (map[string]string, error) {
	out := map[string]string{}
	size := 0
	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") {
			v := strings.Join(vs, ",")
			out[lk] = v
			size += len(lk) - len("x-amz-meta-") + len(v)
		}
	}
	if size > MaxUserMetadataBytes {
		return nil, ErrMetadataTooLarge
	}
	for _, sh := range systemHeaders {
		if v := h.Get(sh); v != "" {
			out[strings.ToLower(sh)] = v
		}
	}
	// aws-chunked is a transport encoding, never part of the object.
	if ce, ok := out["content-encoding"]; ok {
		var keep []string
		for _, p := range strings.Split(ce, ",") {
			if p = strings.TrimSpace(p); p != "" && !strings.EqualFold(p, "aws-chunked") {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			delete(out, "content-encoding")
		} else {
			out["content-encoding"] = strings.Join(keep, ", ")
		}
	}
	return out, nil
}

// ApplyMetadata writes stored metadata back onto a response. User metadata
// keys are emitted lower-case exactly as AWS does, bypassing Go's header
// canonicalisation.
func ApplyMetadata(w http.Header, m map[string]string) {
	for k, v := range m {
		switch {
		case strings.HasPrefix(k, "x-amz-meta-"):
			w[k] = []string{v}
		case isSystemHeader(k):
			w.Set(k, v)
		}
	}
}

func isSystemHeader(k string) bool {
	for _, sh := range systemHeaders {
		if strings.EqualFold(sh, k) {
			return true
		}
	}
	return false
}

// ParseRange parses a Range header against size. ok=false means no range
// header; an error means the range is not satisfiable.
func ParseRange(h string, size int64) (off, length int64, ok bool, err error) {
	if h == "" {
		return 0, size, false, nil
	}
	spec, found := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !found || strings.Contains(spec, ",") {
		// Unsupported units or multiple ranges: ignore per RFC 7233.
		return 0, size, false, nil
	}
	start, end, found := strings.Cut(spec, "-")
	if !found {
		return 0, size, false, nil
	}
	start, end = strings.TrimSpace(start), strings.TrimSpace(end)
	switch {
	case start == "" && end == "":
		return 0, size, false, nil
	case start == "":
		n, perr := strconv.ParseInt(end, 10, 64)
		if perr != nil || n < 0 {
			return 0, 0, true, errors.New("unsatisfiable")
		}
		if n == 0 || size == 0 {
			return 0, 0, true, errors.New("unsatisfiable")
		}
		if n > size {
			n = size
		}
		return size - n, n, true, nil
	default:
		s, perr := strconv.ParseInt(start, 10, 64)
		if perr != nil || s < 0 {
			return 0, 0, true, errors.New("unsatisfiable")
		}
		if s >= size {
			return 0, 0, true, errors.New("unsatisfiable")
		}
		if end == "" {
			return s, size - s, true, nil
		}
		e, perr := strconv.ParseInt(end, 10, 64)
		if perr != nil || e < s {
			return 0, size, false, nil
		}
		if e >= size {
			e = size - 1
		}
		return s, e - s + 1, true, nil
	}
}

// ContentRange formats a Content-Range header.
func ContentRange(off, length, size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", off, off+length-1, size)
}

// Conditional is the result of evaluating conditional headers.
type Conditional int

// Conditional outcomes.
const (
	CondOK Conditional = iota
	CondNotModified
	CondPreconditionFailed
)

// EvaluateConditions applies If-Match, If-None-Match, If-Modified-Since and
// If-Unmodified-Since for GET/HEAD against an object's ETag and mtime. ETag
// comparison is weak-insensitive and tolerates missing quotes.
func EvaluateConditions(h http.Header, etag string, modTime time.Time) Conditional {
	modTime = modTime.Truncate(time.Second)
	if im := h.Get("If-Match"); im != "" {
		if !etagMatches(im, etag) {
			return CondPreconditionFailed
		}
	} else if ius := h.Get("If-Unmodified-Since"); ius != "" {
		if t, err := http.ParseTime(ius); err == nil && modTime.After(t) {
			return CondPreconditionFailed
		}
	}
	if inm := h.Get("If-None-Match"); inm != "" {
		if etagMatches(inm, etag) {
			return CondNotModified
		}
	} else if ims := h.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !modTime.After(t) {
			return CondNotModified
		}
	}
	return CondOK
}

func etagMatches(header, etag string) bool {
	etag = normalizeETag(etag)
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		if p == "*" {
			return true
		}
		if normalizeETag(p) == etag {
			return true
		}
	}
	return false
}

func normalizeETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	return strings.Trim(s, `"`)
}

// QuoteETag ensures an ETag is wrapped in quotes.
func QuoteETag(s string) string {
	if strings.HasPrefix(s, `"`) {
		return s
	}
	return `"` + s + `"`
}

// CopySource parses an x-amz-copy-source header into bucket, key, versionId.
func CopySource(h string) (bucket, key, versionID string, err error) {
	h = strings.TrimPrefix(h, "/")
	if i := strings.Index(h, "?versionId="); i >= 0 {
		versionID = h[i+len("?versionId="):]
		h = h[:i]
	}
	h = unescapeKey(h)
	bucket, key, found := strings.Cut(h, "/")
	if !found || bucket == "" || key == "" {
		return "", "", "", errors.New("invalid copy source")
	}
	return bucket, key, versionID, nil
}

// FormatHTTPTime renders a time for Last-Modified and Date headers.
func FormatHTTPTime(t time.Time) string {
	return t.UTC().Format(http.TimeFormat)
}

// ParseInt parses a query/header integer with a default and bounds.
func ParseInt(s string, def, min, max int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	if n < min {
		return 0, fmt.Errorf("value %d below minimum %d", n, min)
	}
	if n > max {
		n = max
	}
	return n, nil
}
