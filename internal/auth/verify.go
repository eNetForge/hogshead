package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// credential is a parsed "<ak>/<yyyymmdd>/<region>/<service>/aws4_request".
type credential struct {
	accessKeyID string
	date        time.Time
	region      string
	service     string
}

func (c credential) scope() string { return scopeOf(c.date, c.region, c.service) }

// authParams is the common data carried by both authorization forms.
type authParams struct {
	cred          credential
	signedHeaders []string
	signature     string
}

// Verify authenticates r (header or query form). It does not read the body.
// It uses constant-time signature comparison. For header auth it checks skew;
// for presigned it checks X-Amz-Expires (max 7 days) against now.
func (v *Verifier) Verify(ctx context.Context, r *http.Request) (*Identity, error) {
	if r == nil || r.URL == nil {
		return nil, fmt.Errorf("%w: nil request", ErrMalformed)
	}
	if authz := r.Header.Get(headerAuthorization); authz != "" {
		return v.verifyHeader(ctx, r, authz)
	}
	query := parseRawQuery(r.URL.RawQuery)
	if _, ok := queryLookup(query, queryAlgorithm); ok {
		return v.verifyPresigned(ctx, r, query)
	}
	if _, ok := queryLookup(query, querySignature); ok {
		return nil, fmt.Errorf("%w: %s without %s", ErrMalformed, querySignature, queryAlgorithm)
	}
	if _, ok := queryLookup(query, "AWSAccessKeyId"); ok {
		if _, ok := queryLookup(query, "Signature"); ok {
			if v.AllowV2 {
				return v.verifyV2Query(ctx, r, query)
			}
			return nil, fmt.Errorf("%w: signature version 2 query authentication", ErrUnsupported)
		}
	}
	return nil, ErrMissingAuth
}

// verifyHeader handles the Authorization header form.
func (v *Verifier) verifyHeader(ctx context.Context, r *http.Request, authz string) (*Identity, error) {
	if rest, ok := strings.CutPrefix(strings.TrimSpace(authz), "AWS "); ok && v.AllowV2 {
		return v.verifyV2Header(ctx, r, rest)
	}
	params, err := parseAuthorization(authz)
	if err != nil {
		return nil, err
	}
	date, err := requestDate(r)
	if err != nil {
		return nil, err
	}
	if err := v.checkScope(params.cred, date); err != nil {
		return nil, err
	}
	contentSHA256 := r.Header.Get(headerContentSHA256)
	mode, err := payloadModeOf(contentSHA256)
	if err != nil {
		return nil, err
	}
	now := v.now()
	if delta := now.Sub(date); delta > v.skew() || delta < -v.skew() {
		return nil, fmt.Errorf("%w: request time %s, server time %s", ErrSkew, date.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	payloadHash := contentSHA256
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}
	id, err := v.verifySignature(ctx, r, params, date, payloadHash, "")
	if err != nil {
		return nil, err
	}
	id.Payload = mode
	id.ContentSHA256 = contentSHA256
	return id, nil
}

// verifyPresigned handles the query-string (presigned URL) form.
func (v *Verifier) verifyPresigned(ctx context.Context, r *http.Request, query []queryPair) (*Identity, error) {
	get := func(name string) string {
		val, _ := queryLookup(query, name)
		return val
	}
	if alg := get(queryAlgorithm); alg != algorithm {
		return nil, fmt.Errorf("%w: %s=%q", ErrUnsupported, queryAlgorithm, alg)
	}
	params, err := parseAuthParams(get(queryCredential), get(querySignedHeaders), get(querySignature))
	if err != nil {
		return nil, err
	}
	date, err := parseAmzDate(get(queryDate))
	if err != nil {
		return nil, err
	}
	expires, err := parseExpires(get(queryExpires))
	if err != nil {
		return nil, err
	}
	if err := v.checkScope(params.cred, date); err != nil {
		return nil, err
	}
	now := v.now()
	if date.After(now.Add(v.skew())) {
		return nil, fmt.Errorf("%w: request time %s is in the future", ErrSkew, date.Format(time.RFC3339))
	}
	expiry := date.Add(expires)
	if now.After(expiry) {
		return nil, fmt.Errorf("%w: expired at %s", ErrExpired, expiry.Format(time.RFC3339))
	}
	contentSHA256 := r.Header.Get(headerContentSHA256)
	mode, err := payloadModeOf(contentSHA256)
	if err != nil {
		return nil, err
	}
	payloadHash := unsignedPayload
	if h, ok := queryLookup(query, queryContentSHA256); ok && h != "" {
		payloadHash = h
	}
	id, err := v.verifySignature(ctx, r, params, date, payloadHash, querySignature)
	if err != nil {
		return nil, err
	}
	id.Presigned = true
	id.Expires = expiry
	id.Payload = mode
	id.ContentSHA256 = contentSHA256
	return id, nil
}

// checkScope validates the credential date against the request date and the
// region against the optional allow-list.
func (v *Verifier) checkScope(cred credential, date time.Time) error {
	if cred.date.Format(shortDateFormat) != date.UTC().Format(shortDateFormat) {
		return fmt.Errorf("%w: credential date %s does not match request date %s",
			ErrMalformed, cred.date.Format(shortDateFormat), date.UTC().Format(shortDateFormat))
	}
	if len(v.Regions) > 0 && !slices.Contains(v.Regions, cred.region) {
		return fmt.Errorf("%w: region %q is not served here", ErrMalformed, cred.region)
	}
	return nil
}

// verifySignature looks up the secret, recomputes the signature and compares
// it in constant time. On success it returns a partially filled Identity.
func (v *Verifier) verifySignature(ctx context.Context, r *http.Request, p authParams, date time.Time, payloadHash, excludeQuery string) (*Identity, error) {
	if v.Lookup == nil {
		return nil, errors.New("auth: Verifier.Lookup is nil")
	}
	secret, err := v.Lookup(ctx, p.cred.accessKeyID)
	if err != nil {
		if errors.Is(err, ErrUnknownKey) {
			return nil, err
		}
		return nil, fmt.Errorf("lookup access key: %w", err)
	}
	if secret == "" {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, p.cred.accessKeyID)
	}
	if err := requireSignedHeaders(r, p.signedHeaders); err != nil {
		return nil, err
	}
	creq, err := canonicalRequest(r, p.signedHeaders, payloadHash, excludeQuery)
	if err != nil {
		return nil, err
	}
	scope := p.cred.scope()
	key := signingKey(secret, p.cred.date, p.cred.region, p.cred.service)
	expected := signHex(key, stringToSign(date, scope, creq))
	if !equalSignature(expected, p.signature) {
		return nil, ErrSignatureMismatch
	}
	return &Identity{
		AccessKeyID:   p.cred.accessKeyID,
		Region:        p.cred.region,
		Service:       p.cred.service,
		Date:          date,
		SignedHeaders: p.signedHeaders,
		SigningKey:    key,
		SeedSignature: expected,
		Scope:         scope,
		Version:       4,
	}, nil
}

// requireSignedHeaders enforces the headers AWS insists on: host must be
// signed, and every x-amz-* header present must be signed (x-amz-date and
// x-amz-content-sha256 are covered by the string to sign and may be absent
// from the list).
func requireSignedHeaders(r *http.Request, signed []string) error {
	set := map[string]bool{}
	for _, h := range signed {
		set[strings.ToLower(h)] = true
	}
	if !set["host"] {
		return fmt.Errorf("%w: host must be a signed header", ErrMalformed)
	}
	for name := range r.Header {
		ln := strings.ToLower(name)
		if !strings.HasPrefix(ln, "x-amz-") || ln == "x-amz-date" || ln == "x-amz-content-sha256" {
			continue
		}
		if !set[ln] {
			return fmt.Errorf("%w: header %s must be signed", ErrMalformed, ln)
		}
	}
	return nil
}

// parseAuthorization parses an Authorization header of the form
// "AWS4-HMAC-SHA256 Credential=..., SignedHeaders=..., Signature=...".
func parseAuthorization(authz string) (authParams, error) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(authz), " ")
	switch scheme {
	case algorithm:
	case "AWS":
		return authParams{}, fmt.Errorf("%w: signature version 2", ErrUnsupported)
	default:
		return authParams{}, fmt.Errorf("%w: scheme %q", ErrUnsupported, scheme)
	}
	var cred, signed, sig string
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			return authParams{}, fmt.Errorf("%w: component %q", ErrMalformed, part)
		}
		switch strings.TrimSpace(k) {
		case "Credential":
			cred = strings.TrimSpace(val)
		case "SignedHeaders":
			signed = strings.TrimSpace(val)
		case "Signature":
			sig = strings.TrimSpace(val)
		}
	}
	return parseAuthParams(cred, signed, sig)
}

// parseAuthParams validates the three authorization components shared by
// the header and query forms.
func parseAuthParams(cred, signed, sig string) (authParams, error) {
	c, err := parseCredential(cred)
	if err != nil {
		return authParams{}, err
	}
	headers, err := parseSignedHeaders(signed)
	if err != nil {
		return authParams{}, err
	}
	if !isHex(sig, 64) {
		return authParams{}, fmt.Errorf("%w: signature must be 64 hex characters", ErrMalformed)
	}
	return authParams{cred: c, signedHeaders: headers, signature: strings.ToLower(sig)}, nil
}

func parseCredential(s string) (credential, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 5 || parts[4] != scopeSuffix {
		return credential{}, fmt.Errorf("%w: credential must be <access-key>/<date>/<region>/<service>/%s", ErrMalformed, scopeSuffix)
	}
	if parts[0] == "" || parts[2] == "" || parts[3] == "" {
		return credential{}, fmt.Errorf("%w: empty credential component", ErrMalformed)
	}
	date, err := time.Parse(shortDateFormat, parts[1])
	if err != nil {
		return credential{}, fmt.Errorf("%w: credential date %q", ErrMalformed, parts[1])
	}
	return credential{accessKeyID: parts[0], date: date, region: parts[2], service: parts[3]}, nil
}

func parseSignedHeaders(s string) ([]string, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: empty SignedHeaders", ErrMalformed)
	}
	parts := strings.Split(s, ";")
	headers := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			return nil, fmt.Errorf("%w: empty entry in SignedHeaders", ErrMalformed)
		}
		headers = append(headers, p)
	}
	return headers, nil
}

// requestDate returns the request time from X-Amz-Date or, failing that, the
// Date header (header auth form).
func requestDate(r *http.Request) (time.Time, error) {
	if s := r.Header.Get(headerAmzDate); s != "" {
		return parseAmzDate(s)
	}
	if s := r.Header.Get("Date"); s != "" {
		t, err := http.ParseTime(s)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: Date header %q", ErrMalformed, s)
		}
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%w: missing X-Amz-Date or Date header", ErrMalformed)
}

func parseAmzDate(s string) (time.Time, error) {
	t, err := time.Parse(amzDateFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: X-Amz-Date %q", ErrMalformed, s)
	}
	return t, nil
}

// parseExpires validates X-Amz-Expires: an integer number of seconds between
// 1 and 604800 (seven days).
func parseExpires(s string) (time.Duration, error) {
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s %q", ErrMalformed, queryExpires, s)
	}
	d := time.Duration(secs) * time.Second
	if secs < 1 || d > maxPresignExpiry {
		return 0, fmt.Errorf("%w: %s must be between 1 and %d seconds", ErrMalformed, queryExpires, int64(maxPresignExpiry/time.Second))
	}
	return d, nil
}
