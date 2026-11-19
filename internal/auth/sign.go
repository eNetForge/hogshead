package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// unsignableHeaders are never included in SignedHeaders by Sign: they are
// hop-by-hop or rewritten by Go's HTTP client on the wire.
var unsignableHeaders = map[string]bool{
	"authorization":     true,
	"connection":        true,
	"content-length":    true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"te":                true,
	"trailer":           true,
	"transfer-encoding": true,
	"upgrade":           true,
}

// presignParams are removed from an existing query before presigning so a
// URL can be re-presigned.
var presignParams = []string{
	queryAlgorithm, queryCredential, queryDate, queryExpires, querySignedHeaders, querySignature,
}

func validateSignInputs(req *http.Request, c Credentials, region, service string) error {
	switch {
	case req == nil || req.URL == nil:
		return errors.New("auth: nil request")
	case c.AccessKeyID == "" || c.SecretKey == "":
		return errors.New("auth: empty credentials")
	case region == "" || service == "":
		return errors.New("auth: region and service are required")
	}
	return nil
}

// Sign adds Authorization, X-Amz-Date (and x-amz-content-sha256 if
// payloadHash is non-empty and the header is absent) to req. All headers
// already present on req (plus host) are signed, except hop-by-hop ones.
func Sign(req *http.Request, c Credentials, region, service string, now time.Time, payloadHash string) error {
	if err := validateSignInputs(req, c, region, service); err != nil {
		return err
	}
	now = now.UTC()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set(headerAmzDate, now.Format(amzDateFormat))
	if payloadHash != "" && req.Header.Get(headerContentSHA256) == "" {
		req.Header.Set(headerContentSHA256, payloadHash)
	}
	// The verifier uses the header value as the payload hash, so sign over
	// exactly that (falling back to UNSIGNED-PAYLOAD when the header is absent).
	payloadHash = req.Header.Get(headerContentSHA256)
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}
	signed := signableHeaderNames(req.Header)
	creq, err := canonicalRequest(req, signed, payloadHash, "")
	if err != nil {
		return err
	}
	scope := scopeOf(now, region, service)
	key := signingKey(c.SecretKey, now, region, service)
	sig := signHex(key, stringToSign(now, scope, creq))
	req.Header.Set(headerAuthorization, fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, c.AccessKeyID, scope, strings.Join(signed, ";"), sig))
	return nil
}

// signableHeaderNames returns "host" plus the lowercase names of all headers
// in h that can be signed, sorted.
func signableHeaderNames(h http.Header) []string {
	names := []string{"host"}
	for k := range h {
		lk := strings.ToLower(k)
		if !unsignableHeaders[lk] && lk != "host" {
			names = append(names, lk)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Presign returns a copy of req.URL carrying X-Amz-* query parameters that
// authenticate the request for the given expiry (1s to 7 days). Only the host
// header is signed and the payload is UNSIGNED-PAYLOAD, as S3 clients expect.
// req is not modified.
func Presign(req *http.Request, c Credentials, region, service string, now time.Time, expires time.Duration) (*url.URL, error) {
	if err := validateSignInputs(req, c, region, service); err != nil {
		return nil, err
	}
	if expires < minPresignExpiry || expires > maxPresignExpiry {
		return nil, fmt.Errorf("auth: expiry %s out of range [%s, %s]", expires, minPresignExpiry, maxPresignExpiry)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if host == "" {
		return nil, errors.New("auth: request has no host")
	}
	now = now.UTC()
	scope := scopeOf(now, region, service)

	pairs := make([]queryPair, 0, 8)
	for _, p := range parseRawQuery(req.URL.RawQuery) {
		if !slices.ContainsFunc(presignParams, func(n string) bool { return strings.EqualFold(n, p.key) }) {
			pairs = append(pairs, p)
		}
	}
	pairs = append(pairs,
		queryPair{queryAlgorithm, algorithm},
		queryPair{queryCredential, c.AccessKeyID + "/" + scope},
		queryPair{queryDate, now.Format(amzDateFormat)},
		queryPair{queryExpires, strconv.FormatInt(int64(expires/time.Second), 10)},
		queryPair{querySignedHeaders, "host"},
	)

	u := *req.URL
	u.RawQuery = encodeQuery(pairs)
	signReq := req.Clone(req.Context())
	signReq.URL = &u
	signReq.Host = host

	creq, err := canonicalRequest(signReq, []string{"host"}, unsignedPayload, querySignature)
	if err != nil {
		return nil, err
	}
	key := signingKey(c.SecretKey, now, region, service)
	sig := signHex(key, stringToSign(now, scope, creq))
	u.RawQuery = encodeQuery(append(pairs, queryPair{querySignature, sig}))
	return &u, nil
}
