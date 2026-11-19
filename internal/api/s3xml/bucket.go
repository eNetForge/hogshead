package s3xml

import "encoding/xml"

// Owner identifies a bucket or object owner.
type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName,omitempty"`
}

// Bucket is one entry in ListAllMyBucketsResult.
type Bucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
	BucketRegion string `xml:"BucketRegion,omitempty"`
}

// ListAllMyBucketsResult is the ListBuckets response.
type ListAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Owner   Owner    `xml:"Owner"`
	Buckets []Bucket `xml:"Buckets>Bucket"`
	// ContinuationToken is only present for paginated ListBuckets (2024+).
	ContinuationToken string `xml:"ContinuationToken,omitempty"`
	Prefix            string `xml:"Prefix,omitempty"`
}

// CommonPrefix is a rolled-up prefix emitted when a delimiter is used.
type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// Object is one entry in ListBucketResult.Contents.
type Object struct {
	Key               string   `xml:"Key"`
	LastModified      string   `xml:"LastModified"`
	ETag              string   `xml:"ETag"`
	ChecksumAlgorithm []string `xml:"ChecksumAlgorithm,omitempty"`
	ChecksumType      string   `xml:"ChecksumType,omitempty"`
	Size              int64    `xml:"Size"`
	Owner             *Owner   `xml:"Owner,omitempty"`
	StorageClass      string   `xml:"StorageClass"`
}

// ListBucketResult serves both ListObjects (V1) and ListObjectsV2. V1-only
// and V2-only fields are omitempty so a handler fills whichever apply.
type ListBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Xmlns                 string         `xml:"xmlns,attr,omitempty"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Marker                *string        `xml:"Marker,omitempty"`
	NextMarker            string         `xml:"NextMarker,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	KeyCount              *int           `xml:"KeyCount,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []Object       `xml:"Contents"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes"`
}

// ObjectVersion is one <Version> entry in ListVersionsResult.
type ObjectVersion struct {
	Key               string   `xml:"Key"`
	VersionID         string   `xml:"VersionId"`
	IsLatest          bool     `xml:"IsLatest"`
	LastModified      string   `xml:"LastModified"`
	ETag              string   `xml:"ETag"`
	ChecksumAlgorithm []string `xml:"ChecksumAlgorithm,omitempty"`
	Size              int64    `xml:"Size"`
	Owner             *Owner   `xml:"Owner,omitempty"`
	StorageClass      string   `xml:"StorageClass"`
}

// DeleteMarkerEntry is one <DeleteMarker> entry in ListVersionsResult.
type DeleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	Owner        *Owner `xml:"Owner,omitempty"`
}

// ListVersionsResult is the ListObjectVersions response. AWS interleaves
// Version and DeleteMarker entries in key order; encoding/xml groups the two
// slices, which clients accept.
type ListVersionsResult struct {
	XMLName             xml.Name            `xml:"ListVersionsResult"`
	Xmlns               string              `xml:"xmlns,attr,omitempty"`
	Name                string              `xml:"Name"`
	Prefix              string              `xml:"Prefix"`
	KeyMarker           string              `xml:"KeyMarker"`
	VersionIDMarker     string              `xml:"VersionIdMarker"`
	NextKeyMarker       string              `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string              `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int                 `xml:"MaxKeys"`
	Delimiter           string              `xml:"Delimiter,omitempty"`
	EncodingType        string              `xml:"EncodingType,omitempty"`
	IsTruncated         bool                `xml:"IsTruncated"`
	Version             []ObjectVersion     `xml:"Version"`
	DeleteMarker        []DeleteMarkerEntry `xml:"DeleteMarker"`
	CommonPrefixes      []CommonPrefix      `xml:"CommonPrefixes"`
}

// LocationConstraint is the GetBucketLocation response; an empty Location
// means us-east-1.
type LocationConstraint struct {
	XMLName  xml.Name `xml:"LocationConstraint"`
	Xmlns    string   `xml:"xmlns,attr,omitempty"`
	Location string   `xml:",chardata"`
}

// CreateBucketConfiguration is the optional CreateBucket request body.
type CreateBucketConfiguration struct {
	XMLName            xml.Name `xml:"CreateBucketConfiguration"`
	Xmlns              string   `xml:"xmlns,attr,omitempty"`
	LocationConstraint string   `xml:"LocationConstraint,omitempty"`
}

// VersioningConfiguration is the Put/GetBucketVersioning body.
type VersioningConfiguration struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	Xmlns     string   `xml:"xmlns,attr,omitempty"`
	Status    string   `xml:"Status,omitempty"`
	MfaDelete string   `xml:"MfaDelete,omitempty"`
}

// Versioning status values.
const (
	VersioningEnabled   = "Enabled"
	VersioningSuspended = "Suspended"
)
