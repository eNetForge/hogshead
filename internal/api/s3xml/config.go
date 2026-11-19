package s3xml

import "encoding/xml"

// CORSRule is one <CORSRule> in CORSConfiguration.
type CORSRule struct {
	ID            string   `xml:"ID,omitempty"`
	AllowedOrigin []string `xml:"AllowedOrigin"`
	AllowedMethod []string `xml:"AllowedMethod"`
	AllowedHeader []string `xml:"AllowedHeader,omitempty"`
	ExposeHeader  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds *int     `xml:"MaxAgeSeconds,omitempty"`
}

// CORSConfiguration is the Put/GetBucketCors body.
type CORSConfiguration struct {
	XMLName  xml.Name   `xml:"CORSConfiguration"`
	Xmlns    string     `xml:"xmlns,attr,omitempty"`
	CORSRule []CORSRule `xml:"CORSRule"`
}

// Tag is a key/value pair used by Tagging and lifecycle filters.
type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// Tagging is the Put/GetBucketTagging and Put/GetObjectTagging body.
type Tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	TagSet  []Tag    `xml:"TagSet>Tag"`
}

// LifecycleAnd is the <And> block of a lifecycle filter.
type LifecycleAnd struct {
	Prefix                string `xml:"Prefix,omitempty"`
	Tag                   []Tag  `xml:"Tag,omitempty"`
	ObjectSizeGreaterThan *int64 `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64 `xml:"ObjectSizeLessThan,omitempty"`
}

// LifecycleFilter selects the objects a lifecycle rule applies to.
type LifecycleFilter struct {
	Prefix                string        `xml:"Prefix,omitempty"`
	Tag                   *Tag          `xml:"Tag,omitempty"`
	And                   *LifecycleAnd `xml:"And,omitempty"`
	ObjectSizeGreaterThan *int64        `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64        `xml:"ObjectSizeLessThan,omitempty"`
}

// Expiration is the <Expiration> action of a lifecycle rule.
type Expiration struct {
	Days                      *int   `xml:"Days,omitempty"`
	Date                      string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker *bool  `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

// NoncurrentVersionExpiration is the matching action for noncurrent versions.
type NoncurrentVersionExpiration struct {
	NoncurrentDays          *int `xml:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions *int `xml:"NewerNoncurrentVersions,omitempty"`
}

// AbortIncompleteMultipartUpload aborts stale multipart uploads.
type AbortIncompleteMultipartUpload struct {
	DaysAfterInitiation *int `xml:"DaysAfterInitiation,omitempty"`
}

// LifecycleRule is one <Rule> in LifecycleConfiguration. Transition and
// NoncurrentVersionTransition are intentionally omitted (single storage class).
type LifecycleRule struct {
	ID                             string                          `xml:"ID,omitempty"`
	Status                         string                          `xml:"Status"`
	Filter                         *LifecycleFilter                `xml:"Filter,omitempty"`
	Prefix                         string                          `xml:"Prefix,omitempty"`
	Expiration                     *Expiration                     `xml:"Expiration,omitempty"`
	NoncurrentVersionExpiration    *NoncurrentVersionExpiration    `xml:"NoncurrentVersionExpiration,omitempty"`
	AbortIncompleteMultipartUpload *AbortIncompleteMultipartUpload `xml:"AbortIncompleteMultipartUpload,omitempty"`
}

// LifecycleConfiguration is the Put/GetBucketLifecycleConfiguration body.
type LifecycleConfiguration struct {
	XMLName xml.Name        `xml:"LifecycleConfiguration"`
	Xmlns   string          `xml:"xmlns,attr,omitempty"`
	Rule    []LifecycleRule `xml:"Rule"`
}

// DefaultRetention is the bucket-wide default Object Lock retention.
type DefaultRetention struct {
	Mode  string `xml:"Mode"`
	Days  *int   `xml:"Days,omitempty"`
	Years *int   `xml:"Years,omitempty"`
}

// ObjectLockRule wraps DefaultRetention.
type ObjectLockRule struct {
	DefaultRetention DefaultRetention `xml:"DefaultRetention"`
}

// ObjectLockConfiguration is the Put/GetObjectLockConfiguration body.
type ObjectLockConfiguration struct {
	XMLName           xml.Name        `xml:"ObjectLockConfiguration"`
	Xmlns             string          `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string          `xml:"ObjectLockEnabled,omitempty"`
	Rule              *ObjectLockRule `xml:"Rule,omitempty"`
}

// Retention is the Put/GetObjectRetention body.
type Retention struct {
	XMLName         xml.Name `xml:"Retention"`
	Xmlns           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode"`
	RetainUntilDate string   `xml:"RetainUntilDate"`
}

// LegalHold is the Put/GetObjectLegalHold body.
type LegalHold struct {
	XMLName xml.Name `xml:"LegalHold"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

// ApplyServerSideEncryptionByDefault names the default encryption.
type ApplyServerSideEncryptionByDefault struct {
	SSEAlgorithm   string `xml:"SSEAlgorithm"`
	KMSMasterKeyID string `xml:"KMSMasterKeyID,omitempty"`
}

// ServerSideEncryptionRule is one <Rule> in ServerSideEncryptionConfiguration.
type ServerSideEncryptionRule struct {
	ApplyServerSideEncryptionByDefault *ApplyServerSideEncryptionByDefault `xml:"ApplyServerSideEncryptionByDefault,omitempty"`
	BucketKeyEnabled                   *bool                               `xml:"BucketKeyEnabled,omitempty"`
}

// ServerSideEncryptionConfiguration is the Put/GetBucketEncryption body;
// hogshead accepts and echoes it.
type ServerSideEncryptionConfiguration struct {
	XMLName xml.Name                   `xml:"ServerSideEncryptionConfiguration"`
	Xmlns   string                     `xml:"xmlns,attr,omitempty"`
	Rule    []ServerSideEncryptionRule `xml:"Rule"`
}

// PublicAccessBlockConfiguration is the Put/GetPublicAccessBlock body.
type PublicAccessBlockConfiguration struct {
	XMLName               xml.Name `xml:"PublicAccessBlockConfiguration"`
	Xmlns                 string   `xml:"xmlns,attr,omitempty"`
	BlockPublicAcls       bool     `xml:"BlockPublicAcls"`
	IgnorePublicAcls      bool     `xml:"IgnorePublicAcls"`
	BlockPublicPolicy     bool     `xml:"BlockPublicPolicy"`
	RestrictPublicBuckets bool     `xml:"RestrictPublicBuckets"`
}

// BucketLoggingStatus is the GetBucketLogging body; always empty (disabled).
type BucketLoggingStatus struct {
	XMLName xml.Name `xml:"BucketLoggingStatus"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
}

// NotificationConfiguration is the GetBucketNotificationConfiguration body;
// always empty (no notifications).
type NotificationConfiguration struct {
	XMLName            xml.Name             `xml:"NotificationConfiguration"`
	Xmlns              string               `xml:"xmlns,attr,omitempty"`
	QueueConfiguration []QueueConfiguration `xml:"QueueConfiguration,omitempty"`
}

// QueueConfiguration routes events to a destination ARN. hogshead accepts
// webhook destinations of the form arn:hogshead:webhook:::<name>.
type QueueConfiguration struct {
	ID     string              `xml:"Id,omitempty"`
	Queue  string              `xml:"Queue"`
	Event  []string            `xml:"Event"`
	Filter *NotificationFilter `xml:"Filter,omitempty"`
}

// NotificationFilter narrows events by key prefix and suffix.
type NotificationFilter struct {
	S3Key struct {
		FilterRule []FilterRule `xml:"FilterRule"`
	} `xml:"S3Key"`
}

// FilterRule is a prefix or suffix rule.
type FilterRule struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

// RequestPaymentConfiguration is the Get/PutBucketRequestPayment body.
type RequestPaymentConfiguration struct {
	XMLName xml.Name `xml:"RequestPaymentConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Payer   string   `xml:"Payer"`
}
