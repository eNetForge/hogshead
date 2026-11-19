// Package api implements the S3 HTTP API: routing, request parsing, handlers
// and response encoding.
package api

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Target is the bucket and key a request addresses.
type Target struct {
	Bucket string
	Key    string
	// VirtualHost is true when the bucket came from the Host header.
	VirtualHost bool
}

// ResolveTarget extracts bucket and key using virtual-host style when the
// Host is a subdomain of one of hostnames, otherwise path style.
func ResolveTarget(r *http.Request, hostnames []string) Target {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	path := r.URL.EscapedPath()
	for _, hn := range hostnames {
		hn = strings.ToLower(hn)
		if hn == "" || host == hn {
			continue
		}
		if strings.HasSuffix(host, "."+hn) {
			bucket := strings.TrimSuffix(host, "."+hn)
			if !strings.Contains(bucket, ".") || ValidBucketName(bucket) {
				return Target{Bucket: bucket, Key: unescapeKey(strings.TrimPrefix(path, "/")), VirtualHost: true}
			}
		}
	}
	p := strings.TrimPrefix(path, "/")
	if p == "" {
		return Target{}
	}
	bucket, key, _ := strings.Cut(p, "/")
	return Target{Bucket: unescapeKey(bucket), Key: unescapeKey(key)}
}

// unescapeKey decodes a percent-encoded path segment; '+' is kept literal
// because it is a valid key character in a path.
func unescapeKey(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	u, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return u
}

var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var ipLikeRe = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

// ValidBucketName applies the AWS naming rules for general purpose buckets.
func ValidBucketName(b string) bool {
	if !bucketNameRe.MatchString(b) {
		return false
	}
	if strings.Contains(b, "..") || strings.Contains(b, ".-") || strings.Contains(b, "-.") {
		return false
	}
	if ipLikeRe.MatchString(b) {
		return false
	}
	if strings.HasPrefix(b, "xn--") || strings.HasPrefix(b, "sthree-") || strings.HasSuffix(b, "-s3alias") || strings.HasSuffix(b, "--ol-s3") {
		return false
	}
	return true
}

// MaxKeyLength is the S3 key limit in bytes.
const MaxKeyLength = 1024

// ValidKey reports whether a key is acceptable.
func ValidKey(k string) bool {
	return k != "" && len(k) <= MaxKeyLength
}

// Operation names, used for routing, logging and authorization mapping.
type Operation string

// Operations.
const (
	OpListBuckets Operation = "ListBuckets"

	OpCreateBucket            Operation = "CreateBucket"
	OpDeleteBucket            Operation = "DeleteBucket"
	OpHeadBucket              Operation = "HeadBucket"
	OpListObjects             Operation = "ListObjects"
	OpListObjectsV2           Operation = "ListObjectsV2"
	OpListObjectVersions      Operation = "ListObjectVersions"
	OpListMultipartUploads    Operation = "ListMultipartUploads"
	OpGetBucketLocation       Operation = "GetBucketLocation"
	OpGetBucketVersioning     Operation = "GetBucketVersioning"
	OpPutBucketVersioning     Operation = "PutBucketVersioning"
	OpGetBucketPolicy         Operation = "GetBucketPolicy"
	OpPutBucketPolicy         Operation = "PutBucketPolicy"
	OpDeleteBucketPolicy      Operation = "DeleteBucketPolicy"
	OpGetBucketPolicyStatus   Operation = "GetBucketPolicyStatus"
	OpGetBucketCors           Operation = "GetBucketCors"
	OpPutBucketCors           Operation = "PutBucketCors"
	OpDeleteBucketCors        Operation = "DeleteBucketCors"
	OpGetBucketLifecycle      Operation = "GetBucketLifecycle"
	OpPutBucketLifecycle      Operation = "PutBucketLifecycle"
	OpDeleteBucketLifecycle   Operation = "DeleteBucketLifecycle"
	OpGetBucketAcl            Operation = "GetBucketAcl"
	OpPutBucketAcl            Operation = "PutBucketAcl"
	OpGetBucketTagging        Operation = "GetBucketTagging"
	OpPutBucketTagging        Operation = "PutBucketTagging"
	OpDeleteBucketTagging     Operation = "DeleteBucketTagging"
	OpGetBucketEncryption     Operation = "GetBucketEncryption"
	OpPutBucketEncryption     Operation = "PutBucketEncryption"
	OpDeleteBucketEncryption  Operation = "DeleteBucketEncryption"
	OpGetObjectLockConfig     Operation = "GetObjectLockConfiguration"
	OpPutObjectLockConfig     Operation = "PutObjectLockConfiguration"
	OpGetPublicAccessBlock    Operation = "GetPublicAccessBlock"
	OpPutPublicAccessBlock    Operation = "PutPublicAccessBlock"
	OpDeletePublicAccessBlock Operation = "DeletePublicAccessBlock"
	OpGetBucketOwnership      Operation = "GetBucketOwnershipControls"
	OpPutBucketOwnership      Operation = "PutBucketOwnershipControls"
	OpGetBucketLogging        Operation = "GetBucketLogging"
	OpGetBucketNotification   Operation = "GetBucketNotification"
	OpPutBucketNotification   Operation = "PutBucketNotification"
	OpGetBucketReplication    Operation = "GetBucketReplication"
	OpDeleteObjects           Operation = "DeleteObjects"
	OpPostObject              Operation = "PostObject"

	OpGetObject           Operation = "GetObject"
	OpHeadObject          Operation = "HeadObject"
	OpPutObject           Operation = "PutObject"
	OpCopyObject          Operation = "CopyObject"
	OpDeleteObject        Operation = "DeleteObject"
	OpGetObjectAcl        Operation = "GetObjectAcl"
	OpPutObjectAcl        Operation = "PutObjectAcl"
	OpGetObjectTagging    Operation = "GetObjectTagging"
	OpPutObjectTagging    Operation = "PutObjectTagging"
	OpDeleteObjectTagging Operation = "DeleteObjectTagging"
	OpGetObjectRetention  Operation = "GetObjectRetention"
	OpPutObjectRetention  Operation = "PutObjectRetention"
	OpGetObjectLegalHold  Operation = "GetObjectLegalHold"
	OpPutObjectLegalHold  Operation = "PutObjectLegalHold"
	OpGetObjectAttributes Operation = "GetObjectAttributes"
	OpCreateMultipart     Operation = "CreateMultipartUpload"
	OpUploadPart          Operation = "UploadPart"
	OpUploadPartCopy      Operation = "UploadPartCopy"
	OpCompleteMultipart   Operation = "CompleteMultipartUpload"
	OpAbortMultipart      Operation = "AbortMultipartUpload"
	OpListParts           Operation = "ListParts"
	OpOptions             Operation = "Options"
	OpUnknown             Operation = "Unknown"
)

// Route determines the operation from method, target and query parameters.
func Route(method string, t Target, q url.Values) Operation {
	has := func(k string) bool { _, ok := q[k]; return ok }
	if method == http.MethodOptions {
		return OpOptions
	}
	if t.Bucket == "" {
		if method == http.MethodGet {
			return OpListBuckets
		}
		return OpUnknown
	}
	if t.Key == "" {
		switch method {
		case http.MethodGet:
			switch {
			case has("location"):
				return OpGetBucketLocation
			case has("versioning"):
				return OpGetBucketVersioning
			case has("policy"):
				return OpGetBucketPolicy
			case has("policyStatus"):
				return OpGetBucketPolicyStatus
			case has("cors"):
				return OpGetBucketCors
			case has("lifecycle"):
				return OpGetBucketLifecycle
			case has("acl"):
				return OpGetBucketAcl
			case has("tagging"):
				return OpGetBucketTagging
			case has("encryption"):
				return OpGetBucketEncryption
			case has("object-lock"):
				return OpGetObjectLockConfig
			case has("publicAccessBlock"):
				return OpGetPublicAccessBlock
			case has("ownershipControls"):
				return OpGetBucketOwnership
			case has("logging"):
				return OpGetBucketLogging
			case has("notification"):
				return OpGetBucketNotification
			case has("replication"):
				return OpGetBucketReplication
			case has("uploads"):
				return OpListMultipartUploads
			case has("versions"):
				return OpListObjectVersions
			case q.Get("list-type") == "2":
				return OpListObjectsV2
			default:
				return OpListObjects
			}
		case http.MethodPut:
			switch {
			case has("versioning"):
				return OpPutBucketVersioning
			case has("policy"):
				return OpPutBucketPolicy
			case has("cors"):
				return OpPutBucketCors
			case has("lifecycle"):
				return OpPutBucketLifecycle
			case has("acl"):
				return OpPutBucketAcl
			case has("tagging"):
				return OpPutBucketTagging
			case has("encryption"):
				return OpPutBucketEncryption
			case has("object-lock"):
				return OpPutObjectLockConfig
			case has("publicAccessBlock"):
				return OpPutPublicAccessBlock
			case has("ownershipControls"):
				return OpPutBucketOwnership
			case has("notification"):
				return OpPutBucketNotification
			default:
				return OpCreateBucket
			}
		case http.MethodDelete:
			switch {
			case has("policy"):
				return OpDeleteBucketPolicy
			case has("cors"):
				return OpDeleteBucketCors
			case has("lifecycle"):
				return OpDeleteBucketLifecycle
			case has("tagging"):
				return OpDeleteBucketTagging
			case has("encryption"):
				return OpDeleteBucketEncryption
			case has("publicAccessBlock"):
				return OpDeletePublicAccessBlock
			default:
				return OpDeleteBucket
			}
		case http.MethodHead:
			return OpHeadBucket
		case http.MethodPost:
			if has("delete") {
				return OpDeleteObjects
			}
			return OpPostObject
		}
		return OpUnknown
	}
	switch method {
	case http.MethodGet:
		switch {
		case has("uploadId"):
			return OpListParts
		case has("acl"):
			return OpGetObjectAcl
		case has("tagging"):
			return OpGetObjectTagging
		case has("retention"):
			return OpGetObjectRetention
		case has("legal-hold"):
			return OpGetObjectLegalHold
		case has("attributes"):
			return OpGetObjectAttributes
		default:
			return OpGetObject
		}
	case http.MethodHead:
		return OpHeadObject
	case http.MethodPut:
		switch {
		case has("uploadId") && has("partNumber"):
			if hasHeader(q, "x-amz-copy-source") {
				return OpUploadPartCopy
			}
			return OpUploadPart
		case has("acl"):
			return OpPutObjectAcl
		case has("tagging"):
			return OpPutObjectTagging
		case has("retention"):
			return OpPutObjectRetention
		case has("legal-hold"):
			return OpPutObjectLegalHold
		case hasHeader(q, "x-amz-copy-source"):
			return OpCopyObject
		default:
			return OpPutObject
		}
	case http.MethodPost:
		switch {
		case has("uploads"):
			return OpCreateMultipart
		case has("uploadId"):
			return OpCompleteMultipart
		}
		return OpUnknown
	case http.MethodDelete:
		switch {
		case has("uploadId"):
			return OpAbortMultipart
		case has("tagging"):
			return OpDeleteObjectTagging
		default:
			return OpDeleteObject
		}
	}
	return OpUnknown
}

// headerMarker is how Route learns about headers: callers add "\x00header:<name>"
// pseudo-keys to the query via WithHeaders. This keeps Route a pure function.
const headerMarker = "\x00header:"

// WithHeaders copies routing-relevant headers into a query copy for Route.
func WithHeaders(q url.Values, h http.Header) url.Values {
	out := make(url.Values, len(q)+1)
	for k, v := range q {
		out[k] = v
	}
	if h.Get("x-amz-copy-source") != "" {
		out[headerMarker+"x-amz-copy-source"] = []string{"1"}
	}
	return out
}

func hasHeader(q url.Values, name string) bool {
	_, ok := q[headerMarker+name]
	return ok
}

// ActionFor maps an operation to the IAM action used for authorization.
// versioned selects the *Version variants when a versionId is present.
func ActionFor(op Operation, versioned ...bool) string {
	if len(versioned) > 0 && versioned[0] {
		switch op {
		case OpGetObject, OpHeadObject:
			return "s3:GetObjectVersion"
		case OpDeleteObject:
			return "s3:DeleteObjectVersion"
		case OpGetObjectTagging:
			return "s3:GetObjectVersionTagging"
		case OpPutObjectTagging, OpDeleteObjectTagging:
			return "s3:PutObjectVersionTagging"
		case OpGetObjectAcl:
			return "s3:GetObjectVersionAcl"
		case OpPutObjectAcl:
			return "s3:PutObjectVersionAcl"
		case OpGetObjectAttributes:
			return "s3:GetObjectVersionAttributes"
		}
	}
	switch op {
	case OpListBuckets:
		return "s3:ListAllMyBuckets"
	case OpCreateBucket:
		return "s3:CreateBucket"
	case OpDeleteBucket:
		return "s3:DeleteBucket"
	case OpHeadBucket, OpListObjects, OpListObjectsV2:
		return "s3:ListBucket"
	case OpListObjectVersions:
		return "s3:ListBucketVersions"
	case OpListMultipartUploads:
		return "s3:ListBucketMultipartUploads"
	case OpGetBucketLocation:
		return "s3:GetBucketLocation"
	case OpGetBucketVersioning:
		return "s3:GetBucketVersioning"
	case OpPutBucketVersioning:
		return "s3:PutBucketVersioning"
	case OpGetBucketPolicy, OpGetBucketPolicyStatus:
		return "s3:GetBucketPolicy"
	case OpPutBucketPolicy:
		return "s3:PutBucketPolicy"
	case OpDeleteBucketPolicy:
		return "s3:DeleteBucketPolicy"
	case OpGetBucketCors:
		return "s3:GetBucketCORS"
	case OpPutBucketCors, OpDeleteBucketCors:
		return "s3:PutBucketCORS"
	case OpGetBucketLifecycle:
		return "s3:GetLifecycleConfiguration"
	case OpPutBucketLifecycle, OpDeleteBucketLifecycle:
		return "s3:PutLifecycleConfiguration"
	case OpGetBucketAcl:
		return "s3:GetBucketAcl"
	case OpPutBucketAcl:
		return "s3:PutBucketAcl"
	case OpGetBucketTagging:
		return "s3:GetBucketTagging"
	case OpPutBucketTagging, OpDeleteBucketTagging:
		return "s3:PutBucketTagging"
	case OpGetBucketEncryption:
		return "s3:GetEncryptionConfiguration"
	case OpPutBucketEncryption, OpDeleteBucketEncryption:
		return "s3:PutEncryptionConfiguration"
	case OpGetObjectLockConfig:
		return "s3:GetBucketObjectLockConfiguration"
	case OpPutObjectLockConfig:
		return "s3:PutBucketObjectLockConfiguration"
	case OpGetPublicAccessBlock:
		return "s3:GetBucketPublicAccessBlock"
	case OpPutPublicAccessBlock, OpDeletePublicAccessBlock:
		return "s3:PutBucketPublicAccessBlock"
	case OpGetBucketOwnership:
		return "s3:GetBucketOwnershipControls"
	case OpPutBucketOwnership:
		return "s3:PutBucketOwnershipControls"
	case OpGetBucketLogging:
		return "s3:GetBucketLogging"
	case OpGetBucketNotification:
		return "s3:GetBucketNotification"
	case OpPutBucketNotification:
		return "s3:PutBucketNotification"
	case OpGetBucketReplication:
		return "s3:GetReplicationConfiguration"
	case OpDeleteObjects, OpDeleteObject:
		return "s3:DeleteObject"
	case OpAbortMultipart:
		return "s3:AbortMultipartUpload"
	case OpGetObject, OpHeadObject, OpGetObjectAttributes:
		return "s3:GetObject"
	case OpPutObject, OpCopyObject, OpPostObject, OpCreateMultipart, OpUploadPart, OpUploadPartCopy, OpCompleteMultipart:
		return "s3:PutObject"
	case OpGetObjectAcl:
		return "s3:GetObjectAcl"
	case OpPutObjectAcl:
		return "s3:PutObjectAcl"
	case OpGetObjectTagging:
		return "s3:GetObjectTagging"
	case OpPutObjectTagging, OpDeleteObjectTagging:
		return "s3:PutObjectTagging"
	case OpGetObjectRetention:
		return "s3:GetObjectRetention"
	case OpPutObjectRetention:
		return "s3:PutObjectRetention"
	case OpGetObjectLegalHold:
		return "s3:GetObjectLegalHold"
	case OpPutObjectLegalHold:
		return "s3:PutObjectLegalHold"
	case OpListParts:
		return "s3:ListMultipartUploadParts"
	}
	return "s3:" + string(op)
}
