package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SigV2 is defined on HMAC-SHA1
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signature Version 2 support. Still emitted by botocore's default
// generate_presigned_url and by s3cmd with signature_v2 = True.

// v2SubResources are the query parameters that belong in the canonical
// resource, per the AWS SigV2 specification.
var v2SubResources = map[string]bool{
	"acl": true, "lifecycle": true, "location": true, "logging": true, "notification": true, "partNumber": true,
	"policy": true, "requestPayment": true, "torrent": true, "uploadId": true, "uploads": true, "versionId": true,
	"versioning": true, "versions": true, "website": true, "delete": true, "cors": true, "tagging": true,
	"restore": true, "response-content-type": true, "response-content-language": true, "response-expires": true,
	"response-cache-control": true, "response-content-disposition": true, "response-content-encoding": true,
	"object-lock": true, "retention": true, "legal-hold": true, "publicAccessBlock": true, "encryption": true,
	"attributes": true, "select": true, "select-type": true, "ownershipControls": true, "policyStatus": true,
	"intelligent-tiering": true, "metrics": true, "analytics": true, "inventory": true, "replication": true,
	"accelerate": true,
}

// verifyV2Header handles "Authorization: AWS <ak>:<sig>".
func (v *Verifier) verifyV2Header(ctx context.Context, r *http.Request, rest string) (*Identity, error) {
	ak, sig, ok := strings.Cut(strings.TrimSpace(rest), ":")
	if !ok || ak == "" || sig == "" {
		return nil, fmt.Errorf("%w: malformed SigV2 Authorization header", ErrMalformed)
	}
	dateStr := r.Header.Get("x-amz-date")
	dateLine := r.Header.Get("Date")
	if dateStr != "" {
		dateLine = "" // x-amz-date is covered by the canonical amz headers
	} else {
		dateStr = dateLine
	}
	if dateStr == "" {
		return nil, fmt.Errorf("%w: SigV2 request needs a Date or x-amz-date header", ErrMalformed)
	}
	date, err := parseV2Date(dateStr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	now := v.now()
	if d := now.Sub(date); d > v.skew() || -d > v.skew() {
		return nil, ErrSkew
	}
	sts := v.v2StringToSign(r, dateLine, false, "")
	return v.v2Finish(ctx, ak, sig, sts, date, false)
}

// verifyV2Query handles ?AWSAccessKeyId=&Expires=&Signature=.
func (v *Verifier) verifyV2Query(ctx context.Context, r *http.Request, query []queryPair) (*Identity, error) {
	get := func(k string) string {
		s, _ := queryLookup(query, k)
		return s
	}
	ak, sig, exp := get("AWSAccessKeyId"), get("Signature"), get("Expires")
	if ak == "" || sig == "" || exp == "" {
		return nil, fmt.Errorf("%w: SigV2 query auth needs AWSAccessKeyId, Expires and Signature", ErrMalformed)
	}
	expires, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid Expires", ErrMalformed)
	}
	expAt := time.Unix(expires, 0)
	if v.now().After(expAt) {
		return nil, ErrExpired
	}
	sts := v.v2StringToSign(r, exp, true, "")
	id, err := v.v2Finish(ctx, ak, sig, sts, v.now(), true)
	if err != nil {
		return nil, err
	}
	id.Expires = expAt
	return id, nil
}

func (v *Verifier) v2Finish(ctx context.Context, ak, sig, sts string, date time.Time, presigned bool) (*Identity, error) {
	if v.Lookup == nil {
		return nil, errors.New("auth: Verifier.Lookup is nil")
	}
	secret, err := v.Lookup(ctx, ak)
	if err != nil {
		if errors.Is(err, ErrUnknownKey) {
			return nil, err
		}
		return nil, fmt.Errorf("lookup access key: %w", err)
	}
	if secret == "" {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, ak)
	}
	expected := SignV2(secret, sts)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return nil, ErrSignatureMismatch
	}
	return &Identity{AccessKeyID: ak, Date: date, Presigned: presigned, Payload: PayloadUnsigned, Version: 2}, nil
}

// SignV2 computes the base64 HMAC-SHA1 signature of a string to sign.
func SignV2(secret, stringToSign string) string {
	m := hmac.New(sha1.New, []byte(secret))
	m.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// v2StringToSign builds the SigV2 string to sign. dateOrExpires is the Date
// line (header form) or the Expires value (query form).
func (v *Verifier) v2StringToSign(r *http.Request, dateOrExpires string, query bool, bucketHint string) string {
	var sb strings.Builder
	sb.WriteString(r.Method)
	sb.WriteString("\n")
	sb.WriteString(r.Header.Get("Content-MD5"))
	sb.WriteString("\n")
	sb.WriteString(r.Header.Get("Content-Type"))
	sb.WriteString("\n")
	sb.WriteString(dateOrExpires)
	sb.WriteString("\n")
	sb.WriteString(v2AmzHeaders(r.Header))
	sb.WriteString(v.v2Resource(r, bucketHint))
	_ = query
	return sb.String()
}

func v2AmzHeaders(h http.Header) string {
	var names []string
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") {
			names = append(names, lk)
		}
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		vals := h.Values(n)
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		sb.WriteString(n)
		sb.WriteString(":")
		sb.WriteString(strings.Join(vals, ","))
		sb.WriteString("\n")
	}
	return sb.String()
}

// v2Resource builds the canonicalized resource: virtual-host bucket prefix,
// the request path and sorted sub-resources.
func (v *Verifier) v2Resource(r *http.Request, bucketHint string) string {
	var sb strings.Builder
	if bucketHint == "" {
		bucketHint = v.virtualHostBucket(r.Host)
	}
	if bucketHint != "" {
		sb.WriteString("/" + bucketHint)
	}
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	sb.WriteString(path)
	query := parseRawQuery(r.URL.RawQuery)
	var subs []string
	for _, p := range query {
		if !v2SubResources[p.key] {
			continue
		}
		if p.value != "" {
			subs = append(subs, p.key+"="+p.value)
		} else {
			subs = append(subs, p.key)
		}
	}
	sort.Strings(subs)
	if len(subs) > 0 {
		sb.WriteString("?" + strings.Join(subs, "&"))
	}
	return sb.String()
}

// virtualHostBucket returns the bucket from a virtual-host style Host.
func (v *Verifier) virtualHostBucket(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	for _, suffix := range v.VirtualHostSuffixes {
		suffix = strings.ToLower(suffix)
		if suffix != "" && host != suffix && strings.HasSuffix(host, "."+suffix) {
			return strings.TrimSuffix(host, "."+suffix)
		}
	}
	return ""
}

func parseV2Date(s string) (time.Time, error) {
	for _, layout := range []string{http.TimeFormat, time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC, amzDateFormat, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date %q", s)
}

// PresignV2 builds a SigV2 query-authenticated URL (used in tests and by
// tooling that must talk to very old clients).
func PresignV2(req *http.Request, c Credentials, expires time.Time, virtualHostSuffixes []string) string {
	v := &Verifier{VirtualHostSuffixes: virtualHostSuffixes}
	exp := strconv.FormatInt(expires.Unix(), 10)
	sts := v.v2StringToSign(req, exp, true, "")
	sig := SignV2(c.SecretKey, sts)
	u := *req.URL
	q := u.Query()
	q.Set("AWSAccessKeyId", c.AccessKeyID)
	q.Set("Expires", exp)
	q.Set("Signature", sig)
	u.RawQuery = q.Encode()
	return u.String()
}
