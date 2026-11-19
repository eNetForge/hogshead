package s3xml

import "encoding/xml"

// CopyObjectResult is the CopyObject response body.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr,omitempty"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
	Checksums
	ChecksumType string `xml:"ChecksumType,omitempty"`
}

// CopyPartResult is the UploadPartCopy response body.
type CopyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	Xmlns        string   `xml:"xmlns,attr,omitempty"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
	Checksums
}

// ObjectIdentifier is one <Object> in a DeleteObjects request.
type ObjectIdentifier struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

// Delete is the DeleteObjects request body.
type Delete struct {
	XMLName xml.Name           `xml:"Delete"`
	Xmlns   string             `xml:"xmlns,attr,omitempty"`
	Quiet   bool               `xml:"Quiet,omitempty"`
	Object  []ObjectIdentifier `xml:"Object"`
}

// DeletedObject is one <Deleted> in DeleteResult.
type DeletedObject struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

// DeleteError is one <Error> in DeleteResult.
type DeleteError struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

// DeleteResult is the DeleteObjects response body.
type DeleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	Xmlns   string          `xml:"xmlns,attr,omitempty"`
	Deleted []DeletedObject `xml:"Deleted"`
	Error   []DeleteError   `xml:"Error"`
}

// PostResponse is the body returned for a browser POST object upload when
// success_action_status is 201.
type PostResponse struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Xmlns    string   `xml:"xmlns,attr,omitempty"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}
