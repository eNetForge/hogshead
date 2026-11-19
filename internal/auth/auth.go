// Package auth implements server-side AWS Signature Version 4 verification
// for an S3-compatible HTTP server, together with the matching client-side
// signer and presigner used by tests and the replication client.
//
// The implementation is written from the public AWS SigV4 specification:
// canonical request construction, the string to sign, the HMAC key
// derivation chain, query-string (presigned) authentication and the
// aws-chunked streaming payload formats (signed chunks, signed trailers and
// unsigned trailers). It intentionally accepts a few lenient variations seen
// in real clients (aws-cli, rclone, restic, s3cmd, boto3) that Amazon S3 also
// accepts.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors returned by Verify and the chunked reader. Callers map
// them to S3 error codes with errors.Is; the returned error may carry extra
// context via wrapping.
var (
	// ErrMissingAuth is returned for an anonymous request: no Authorization
	// header and no X-Amz-Algorithm query parameter.
	ErrMissingAuth = errors.New("no authorization")
	// ErrMalformed is returned when the authorization data cannot be parsed
	// (S3: AuthorizationHeaderMalformed / InvalidArgument).
	ErrMalformed = errors.New("malformed authorization")
	// ErrUnknownKey is returned when the access key id is not known
	// (S3: InvalidAccessKeyId).
	ErrUnknownKey = errors.New("unknown access key")
	// ErrSignatureMismatch is returned when the computed signature differs
	// from the presented one (S3: SignatureDoesNotMatch).
	ErrSignatureMismatch = errors.New("signature mismatch")
	// ErrExpired is returned for a presigned request past its X-Amz-Expires
	// window (S3: AccessDenied "Request has expired").
	ErrExpired = errors.New("request expired")
	// ErrSkew is returned when the request time is outside the allowed clock
	// skew (S3: RequestTimeTooSkewed).
	ErrSkew = errors.New("request time too skewed")
	// ErrUnsupported is returned for non-SigV4 schemes such as Signature
	// Version 2 (S3: InvalidArgument).
	ErrUnsupported = errors.New("unsupported algorithm")
)

// PayloadMode describes how the request body is authenticated, derived from
// the x-amz-content-sha256 header.
type PayloadMode int

const (
	// PayloadSigned: x-amz-content-sha256 is the hex SHA-256 of the body.
	PayloadSigned PayloadMode = iota
	// PayloadUnsigned: UNSIGNED-PAYLOAD (or header absent).
	PayloadUnsigned
	// PayloadStreaming: STREAMING-AWS4-HMAC-SHA256-PAYLOAD, each aws-chunked
	// chunk carries its own signature.
	PayloadStreaming
	// PayloadStreamingTrailer: STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER,
	// signed chunks followed by signed trailer headers.
	PayloadStreamingTrailer
	// PayloadStreamingUnsignedTrailer: STREAMING-UNSIGNED-PAYLOAD-TRAILER,
	// unsigned chunks followed by unsigned trailer headers.
	PayloadStreamingUnsignedTrailer
)

// String returns the x-amz-content-sha256 constant (or a description) for m.
func (m PayloadMode) String() string {
	switch m {
	case PayloadSigned:
		return "SIGNED-PAYLOAD"
	case PayloadUnsigned:
		return unsignedPayload
	case PayloadStreaming:
		return streamingSigned
	case PayloadStreamingTrailer:
		return streamingSignedTrailer
	case PayloadStreamingUnsignedTrailer:
		return streamingUnsignedTrailer
	}
	return "PayloadMode(unknown)"
}

// Streaming reports whether m is one of the aws-chunked modes that require
// NewChunkedReader to decode the body.
func (m PayloadMode) Streaming() bool {
	switch m {
	case PayloadStreaming, PayloadStreamingTrailer, PayloadStreamingUnsignedTrailer:
		return true
	}
	return false
}

// Identity is the result of a successful verification.
type Identity struct {
	AccessKeyID   string
	Region        string
	Service       string
	Date          time.Time // from X-Amz-Date / Date (header form) or X-Amz-Date (query form)
	Presigned     bool
	Expires       time.Time // presigned only
	Payload       PayloadMode
	ContentSHA256 string // raw x-amz-content-sha256 header value
	SignedHeaders []string
	SigningKey    []byte // derived key, needed for chunk signature verification
	SeedSignature string // the request signature (hex), seed for chunk signatures
	Scope         string // "<date>/<region>/<service>/aws4_request"
	// Version is 4 for SigV4 and 2 for SigV2.
	Version int
}

// SecretLookup returns the secret for an access key id, or ErrUnknownKey.
type SecretLookup func(ctx context.Context, accessKeyID string) (string, error)

// Verifier authenticates incoming requests.
type Verifier struct {
	Lookup SecretLookup
	Now    func() time.Time // nil = time.Now
	Skew   time.Duration    // allowed clock skew for header auth, default 15m
	// Regions is an optional allow-list; empty means accept any region string.
	Regions []string
	// VirtualHostSuffixes are the hostnames under which buckets appear as
	// subdomains; needed to build the SigV2 canonical resource.
	VirtualHostSuffixes []string
	// AllowV2 enables Signature Version 2 (header and query forms).
	AllowV2 bool
}

// Credentials is a static access key pair used by Sign and Presign.
type Credentials struct{ AccessKeyID, SecretKey string }

// ChunkedReader decodes an aws-chunked request body. After Read returns
// io.EOF, Trailers returns the trailer headers (x-amz-checksum-*) that
// followed the final chunk.
type ChunkedReader interface {
	io.Reader
	Trailers() http.Header
}

const (
	algorithm        = "AWS4-HMAC-SHA256"
	algorithmPayload = "AWS4-HMAC-SHA256-PAYLOAD"
	algorithmTrailer = "AWS4-HMAC-SHA256-TRAILER"
	scopeSuffix      = "aws4_request"

	amzDateFormat   = "20060102T150405Z"
	shortDateFormat = "20060102"

	unsignedPayload          = "UNSIGNED-PAYLOAD"
	streamingSigned          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingSignedTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"

	// emptySHA256 is the hex SHA-256 of the empty string.
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	headerAuthorization = "Authorization"
	headerAmzDate       = "X-Amz-Date"
	headerContentSHA256 = "X-Amz-Content-Sha256"

	queryAlgorithm     = "X-Amz-Algorithm"
	queryCredential    = "X-Amz-Credential" //nolint:gosec // header constant, not a credential
	queryDate          = "X-Amz-Date"
	queryExpires       = "X-Amz-Expires"
	querySignedHeaders = "X-Amz-SignedHeaders"
	querySignature     = "X-Amz-Signature"
	queryContentSHA256 = "X-Amz-Content-Sha256"

	defaultSkew      = 15 * time.Minute
	minPresignExpiry = time.Second
	maxPresignExpiry = 7 * 24 * time.Hour
)

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now().UTC()
	}
	return time.Now().UTC()
}

func (v *Verifier) skew() time.Duration {
	if v.Skew > 0 {
		return v.Skew
	}
	return defaultSkew
}

// payloadModeOf classifies an x-amz-content-sha256 header value. A missing
// header is treated leniently as UNSIGNED-PAYLOAD.
func payloadModeOf(contentSHA256 string) (PayloadMode, error) {
	switch contentSHA256 {
	case "", unsignedPayload:
		return PayloadUnsigned, nil
	case streamingSigned:
		return PayloadStreaming, nil
	case streamingSignedTrailer:
		return PayloadStreamingTrailer, nil
	case streamingUnsignedTrailer:
		return PayloadStreamingUnsignedTrailer, nil
	}
	if strings.HasPrefix(contentSHA256, "STREAMING-") {
		return 0, fmt.Errorf("%w: x-amz-content-sha256 %q", ErrUnsupported, contentSHA256)
	}
	if !isHex(contentSHA256, 64) {
		return 0, fmt.Errorf("%w: x-amz-content-sha256 must be a hex sha256, UNSIGNED-PAYLOAD or a STREAMING-* value", ErrMalformed)
	}
	return PayloadSigned, nil
}
