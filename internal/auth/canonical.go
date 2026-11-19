package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const hexUpper = "0123456789ABCDEF"

// isUnreserved reports whether c may appear unencoded in an AWS URI-encoded
// string: A-Z a-z 0-9 - _ . ~.
func isUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '~':
		return true
	}
	return false
}

// uriEncode applies AWS URI encoding to s. '/' is kept as-is unless
// encodeSlash is set. Every other non-unreserved byte becomes %XX (uppercase).
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isUnreserved(c), c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0f])
		}
	}
	return b.String()
}

// canonicalURI returns the S3-style canonical URI: each path segment decoded
// and re-encoded exactly once with AWS rules, no path normalization. An empty
// path becomes "/".
func canonicalURI(u *url.URL) string {
	escaped := u.EscapedPath()
	if escaped == "" {
		return "/"
	}
	segments := strings.Split(escaped, "/")
	out := make([]string, len(segments))
	for i, seg := range segments {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			decoded = seg
		}
		out[i] = uriEncode(decoded, true)
	}
	return strings.Join(out, "/")
}

// trimHeaderValue trims leading/trailing whitespace and collapses internal
// runs of whitespace into a single space, as required for canonical headers.
func trimHeaderValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// headerValue returns the canonical value of a (lowercase) signed header.
// Host comes from r.Host; multiple values are joined with ",". Headers that
// Go's server strips from the map are reconstructed from the request.
func headerValue(r *http.Request, name string) (string, bool) {
	if name == "host" {
		host := r.Host
		if host == "" && r.URL != nil {
			host = r.URL.Host
		}
		return trimHeaderValue(host), host != ""
	}
	if vals, ok := lookupHeader(r.Header, name); ok {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = trimHeaderValue(v)
		}
		return strings.Join(parts, ","), true
	}
	switch name {
	case "content-length":
		if r.ContentLength >= 0 {
			return strconv.FormatInt(r.ContentLength, 10), true
		}
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return strings.Join(r.TransferEncoding, ","), true
		}
	case "expect":
		return "100-continue", true
	}
	return "", false
}

// lookupHeader finds a header case-insensitively, trying the canonical key
// first and falling back to a scan for keys the canonicalizer leaves alone.
func lookupHeader(h http.Header, name string) ([]string, bool) {
	if vals, ok := h[textproto.CanonicalMIMEHeaderKey(name)]; ok {
		return vals, true
	}
	for k, vals := range h {
		if strings.EqualFold(k, name) {
			return vals, true
		}
	}
	return nil, false
}

// canonicalHeaders builds the "name:value\n" block for the signed headers,
// in the order given. A signed header missing from the request is malformed.
func canonicalHeaders(r *http.Request, signed []string) (string, error) {
	var b strings.Builder
	for _, name := range signed {
		val, ok := headerValue(r, name)
		if !ok {
			return "", fmt.Errorf("%w: signed header %q missing from request", ErrMalformed, name)
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(val)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// canonicalRequest assembles the SigV4 canonical request. excludeQuery names
// a query parameter to leave out (X-Amz-Signature for presigned requests).
func canonicalRequest(r *http.Request, signed []string, payloadHash, excludeQuery string) (string, error) {
	headers, err := canonicalHeaders(r, signed)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(r.URL),
		canonicalQuery(r.URL.RawQuery, excludeQuery),
		headers,
		strings.Join(signed, ";"),
		payloadHash,
	}, "\n"), nil
}

// scopeOf returns "<yyyymmdd>/<region>/<service>/aws4_request".
func scopeOf(date time.Time, region, service string) string {
	return date.UTC().Format(shortDateFormat) + "/" + region + "/" + service + "/" + scopeSuffix
}

// stringToSign builds the SigV4 string to sign for a canonical request.
func stringToSign(date time.Time, scope, canonicalReq string) string {
	return algorithm + "\n" + date.UTC().Format(amzDateFormat) + "\n" + scope + "\n" + sha256Hex([]byte(canonicalReq))
}

// signingKey derives the per-day signing key:
// HMAC chain over "AWS4"+secret, date, region, service, "aws4_request".
func signingKey(secret string, date time.Time, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date.UTC().Format(shortDateFormat))
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, scopeSuffix)
}

// signHex returns the lowercase hex HMAC-SHA256 of data under key.
func signHex(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, data))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// equalSignature compares two hex signatures in constant time, ignoring
// case. Length is public information (both are 64 hex chars), so a length
// mismatch returning early does not leak anything useful.
func equalSignature(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// isHex reports whether s is exactly n hexadecimal characters.
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
