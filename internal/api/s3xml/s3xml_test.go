package s3xml

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/api/s3err"
)

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%T): %v", v, err)
	}
	// Every document we produce must be re-parseable.
	var sink struct{}
	if err := xml.Unmarshal(b, &sink); err != nil {
		t.Fatalf("Marshal(%T) produced invalid XML: %v\n%s", v, err, b)
	}
	return string(b)
}

// assertRoot checks the declaration, the root element and the S3 namespace.
func assertRoot(t *testing.T, doc, root string) {
	t.Helper()
	if !strings.HasPrefix(doc, xml.Header) {
		t.Fatalf("missing XML header:\n%s", doc)
	}
	open := `<` + root + ` xmlns="` + NS + `">`
	if !strings.Contains(doc, open) {
		// Empty roots render self-closing.
		if !strings.Contains(doc, `<`+root+` xmlns="`+NS+`"></`+root+`>`) {
			t.Fatalf("missing root %q with namespace:\n%s", root, doc)
		}
	}
	if !strings.HasSuffix(doc, "</"+root+">\n") && !strings.HasSuffix(doc, "</"+root+">\n") {
		t.Fatalf("document should end with closing root:\n%s", doc)
	}
}

// assertOrder checks that fragments appear in the given order.
func assertOrder(t *testing.T, doc string, frags ...string) {
	t.Helper()
	pos := 0
	for _, f := range frags {
		i := strings.Index(doc[pos:], f)
		if i < 0 {
			t.Fatalf("fragment %q missing (or out of order) in:\n%s", f, doc)
		}
		pos += i + len(f)
	}
}

func intp(i int) *int       { return &i }
func int64p(i int64) *int64 { return &i }
func boolp(b bool) *bool    { return &b }
func ts(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }

func TestFormatTime(t *testing.T) {
	cases := map[string]string{
		"2009-10-12T17:50:30Z":           "2009-10-12T17:50:30.000Z",
		"2009-10-12T17:50:30.123456789Z": "2009-10-12T17:50:30.123Z",
		"2009-10-12T19:50:30.5+02:00":    "2009-10-12T17:50:30.500Z",
	}
	for in, want := range cases {
		if got := FormatTime(ts(in)); got != want {
			t.Errorf("FormatTime(%s) = %q, want %q", in, got, want)
		}
	}
	back, err := ParseTime("2009-10-12T17:50:30.123Z")
	if err != nil || !back.Equal(ts("2009-10-12T17:50:30.123Z")) {
		t.Fatalf("ParseTime = %v, %v", back, err)
	}
	back, err = ParseTime("2009-10-12T17:50:30Z")
	if err != nil || !back.Equal(ts("2009-10-12T17:50:30Z")) {
		t.Fatalf("ParseTime rfc3339 = %v, %v", back, err)
	}
	if _, err := ParseTime("yesterday"); err == nil {
		t.Fatal("ParseTime should reject garbage")
	}
}

func TestMarshalNamespaceDoesNotMutate(t *testing.T) {
	v := LocationConstraint{Location: "eu-west-1"}
	doc := mustMarshal(t, v)
	assertRoot(t, doc, "LocationConstraint")
	if v.Xmlns != "" {
		t.Fatal("Marshal mutated the caller's value")
	}
	// Pointer input likewise.
	pv := &LocationConstraint{Location: "eu-west-1"}
	mustMarshal(t, pv)
	if pv.Xmlns != "" {
		t.Fatal("Marshal mutated the pointed-to value")
	}
	// Explicit namespace is preserved.
	custom := LocationConstraint{Xmlns: "urn:x", Location: "x"}
	if doc := mustMarshal(t, custom); !strings.Contains(doc, `xmlns="urn:x"`) {
		t.Fatalf("custom xmlns dropped:\n%s", doc)
	}
	// Non-struct and nil pointer inputs pass through untouched.
	if doc := mustMarshal(t, "plain"); !strings.Contains(doc, "<string>plain</string>") {
		t.Fatalf("plain value: %s", doc)
	}
	// A nil pointer is passed through to encoding/xml, which emits nothing.
	var np *LocationConstraint
	if b, err := Marshal(np); err != nil || string(b) != xml.Header+"\n" {
		t.Fatalf("nil pointer: %q, %v", b, err)
	}
	// Struct without an Xmlns field is left as-is.
	type noNS struct {
		XMLName xml.Name `xml:"Foo"`
		A       string
	}
	if doc := mustMarshal(t, noNS{A: "1"}); strings.Contains(doc, "xmlns") {
		t.Fatalf("unexpected xmlns on struct without field: %s", doc)
	}
	if _, err := Marshal(make(chan int)); err == nil {
		t.Fatal("expected error for unsupported type")
	}
}

func TestListAllMyBucketsResult(t *testing.T) {
	doc := mustMarshal(t, ListAllMyBucketsResult{
		Owner:   Owner{ID: "abc", DisplayName: "bara"},
		Buckets: []Bucket{{Name: "one", CreationDate: "2024-01-01T00:00:00.000Z"}, {Name: "two", CreationDate: "2024-01-02T00:00:00.000Z"}},
	})
	assertRoot(t, doc, "ListAllMyBucketsResult")
	assertOrder(t, doc, "<Owner>", "<ID>abc</ID>", "<DisplayName>bara</DisplayName>", "</Owner>",
		"<Buckets>", "<Bucket>", "<Name>one</Name>", "<CreationDate>2024-01-01T00:00:00.000Z</CreationDate>", "</Bucket>",
		"<Bucket>", "<Name>two</Name>", "</Buckets>")
}

func TestListBucketResultV2Order(t *testing.T) {
	doc := mustMarshal(t, ListBucketResult{
		Name:                  "bkt",
		Prefix:                "photos/",
		NextContinuationToken: "tok2",
		StartAfter:            "photos/a",
		KeyCount:              intp(2),
		MaxKeys:               1000,
		Delimiter:             "/",
		EncodingType:          "url",
		IsTruncated:           true,
		Contents: []Object{{
			Key: "photos/a.jpg", LastModified: "2024-01-01T00:00:00.000Z", ETag: `"d41d8cd98f00b204e9800998ecf8427e"`,
			ChecksumAlgorithm: []string{"CRC32"}, Size: 10, Owner: &Owner{ID: "o"}, StorageClass: "STANDARD",
		}},
		CommonPrefixes: []CommonPrefix{{Prefix: "photos/2024/"}},
	})
	assertRoot(t, doc, "ListBucketResult")
	assertOrder(t, doc,
		"<Name>bkt</Name>", "<Prefix>photos/</Prefix>", "<NextContinuationToken>tok2</NextContinuationToken>",
		"<StartAfter>photos/a</StartAfter>", "<KeyCount>2</KeyCount>", "<MaxKeys>1000</MaxKeys>",
		"<Delimiter>/</Delimiter>", "<EncodingType>url</EncodingType>", "<IsTruncated>true</IsTruncated>",
		"<Contents>", "<Key>photos/a.jpg</Key>", "<LastModified>", "<ETag>&#34;d41d8cd98f00b204e9800998ecf8427e&#34;</ETag>",
		"<ChecksumAlgorithm>CRC32</ChecksumAlgorithm>", "<Size>10</Size>", "<Owner>", "<StorageClass>STANDARD</StorageClass>", "</Contents>",
		"<CommonPrefixes>", "<Prefix>photos/2024/</Prefix>", "</CommonPrefixes>")
	for _, absent := range []string{"<Marker>", "<NextMarker>", "<ContinuationToken>"} {
		if strings.Contains(doc, absent) {
			t.Errorf("V1-only/unset element %s should be omitted", absent)
		}
	}
}

func TestListBucketResultV1(t *testing.T) {
	doc := mustMarshal(t, ListBucketResult{Name: "bkt", Marker: strPtr("m"), NextMarker: "n", MaxKeys: 5, IsTruncated: false})
	assertOrder(t, doc, "<Name>bkt</Name>", "<Prefix></Prefix>", "<Marker>m</Marker>", "<NextMarker>n</NextMarker>", "<MaxKeys>5</MaxKeys>", "<IsTruncated>false</IsTruncated>")
	if strings.Contains(doc, "KeyCount") || strings.Contains(doc, "<Contents>") || strings.Contains(doc, "<Owner>") {
		t.Fatalf("unexpected elements in V1 doc:\n%s", doc)
	}
}

func TestListVersionsResult(t *testing.T) {
	doc := mustMarshal(t, ListVersionsResult{
		Name: "bkt", KeyMarker: "", VersionIDMarker: "", MaxKeys: 1000, IsTruncated: false,
		Version:        []ObjectVersion{{Key: "k", VersionID: "v1", IsLatest: true, LastModified: "2024-01-01T00:00:00.000Z", ETag: `"e"`, Size: 1, StorageClass: "STANDARD", Owner: &Owner{ID: "o"}}},
		DeleteMarker:   []DeleteMarkerEntry{{Key: "k", VersionID: "v2", IsLatest: false, LastModified: "2024-01-02T00:00:00.000Z", Owner: &Owner{ID: "o"}}},
		CommonPrefixes: []CommonPrefix{{Prefix: "p/"}},
	})
	assertRoot(t, doc, "ListVersionsResult")
	assertOrder(t, doc, "<Name>bkt</Name>", "<Prefix></Prefix>", "<KeyMarker></KeyMarker>", "<VersionIdMarker></VersionIdMarker>",
		"<MaxKeys>1000</MaxKeys>", "<IsTruncated>false</IsTruncated>",
		"<Version>", "<Key>k</Key>", "<VersionId>v1</VersionId>", "<IsLatest>true</IsLatest>", "<Size>1</Size>", "</Version>",
		"<DeleteMarker>", "<VersionId>v2</VersionId>", "<IsLatest>false</IsLatest>", "</DeleteMarker>",
		"<CommonPrefixes>")
}

func TestBucketConfigResponses(t *testing.T) {
	doc := mustMarshal(t, LocationConstraint{})
	if !strings.Contains(doc, `<LocationConstraint xmlns="`+NS+`"></LocationConstraint>`) {
		t.Fatalf("empty LocationConstraint:\n%s", doc)
	}
	doc = mustMarshal(t, LocationConstraint{Location: "eu-central-1"})
	if !strings.Contains(doc, `>eu-central-1</LocationConstraint>`) {
		t.Fatalf("LocationConstraint chardata:\n%s", doc)
	}

	doc = mustMarshal(t, VersioningConfiguration{})
	if !strings.Contains(doc, `<VersioningConfiguration xmlns="`+NS+`"></VersioningConfiguration>`) {
		t.Fatalf("unset versioning should be an empty element:\n%s", doc)
	}
	doc = mustMarshal(t, VersioningConfiguration{Status: VersioningEnabled, MfaDelete: "Disabled"})
	assertOrder(t, doc, "<Status>Enabled</Status>", "<MfaDelete>Disabled</MfaDelete>")

	doc = mustMarshal(t, PublicAccessBlockConfiguration{BlockPublicAcls: true, RestrictPublicBuckets: true})
	assertRoot(t, doc, "PublicAccessBlockConfiguration")
	assertOrder(t, doc, "<BlockPublicAcls>true</BlockPublicAcls>", "<IgnorePublicAcls>false</IgnorePublicAcls>",
		"<BlockPublicPolicy>false</BlockPublicPolicy>", "<RestrictPublicBuckets>true</RestrictPublicBuckets>")

	assertRoot(t, mustMarshal(t, BucketLoggingStatus{}), "BucketLoggingStatus")
	assertRoot(t, mustMarshal(t, NotificationConfiguration{}), "NotificationConfiguration")
	doc = mustMarshal(t, RequestPaymentConfiguration{Payer: "BucketOwner"})
	assertRoot(t, doc, "RequestPaymentConfiguration")
	assertOrder(t, doc, "<Payer>BucketOwner</Payer>")

	doc = mustMarshal(t, ServerSideEncryptionConfiguration{Rule: []ServerSideEncryptionRule{{
		ApplyServerSideEncryptionByDefault: &ApplyServerSideEncryptionByDefault{SSEAlgorithm: "AES256"},
		BucketKeyEnabled:                   boolp(false),
	}}})
	assertRoot(t, doc, "ServerSideEncryptionConfiguration")
	assertOrder(t, doc, "<Rule>", "<ApplyServerSideEncryptionByDefault>", "<SSEAlgorithm>AES256</SSEAlgorithm>", "</ApplyServerSideEncryptionByDefault>", "<BucketKeyEnabled>false</BucketKeyEnabled>", "</Rule>")
	if strings.Contains(doc, "KMSMasterKeyID") {
		t.Fatal("empty KMSMasterKeyID should be omitted")
	}
}

func TestMultipartResponses(t *testing.T) {
	doc := mustMarshal(t, InitiateMultipartUploadResult{Bucket: "b", Key: "k", UploadID: "u1"})
	assertRoot(t, doc, "InitiateMultipartUploadResult")
	assertOrder(t, doc, "<Bucket>b</Bucket>", "<Key>k</Key>", "<UploadId>u1</UploadId>")

	doc = mustMarshal(t, CompleteMultipartUploadResult{Location: "http://h/b/k", Bucket: "b", Key: "k", ETag: `"e-2"`,
		Checksums: Checksums{ChecksumCRC32C: "abc="}, ChecksumType: "COMPOSITE"})
	assertRoot(t, doc, "CompleteMultipartUploadResult")
	assertOrder(t, doc, "<Location>http://h/b/k</Location>", "<Bucket>b</Bucket>", "<Key>k</Key>", "<ETag>", "<ChecksumCRC32C>abc=</ChecksumCRC32C>", "<ChecksumType>COMPOSITE</ChecksumType>")
	if strings.Contains(doc, "ChecksumSHA1") {
		t.Fatal("empty checksums must be omitted")
	}

	doc = mustMarshal(t, ListPartsResult{Bucket: "b", Key: "k", UploadID: "u", Initiator: &Owner{ID: "i"}, Owner: &Owner{ID: "o"},
		StorageClass: "STANDARD", PartNumberMarker: 0, NextPartNumberMarker: 2, MaxParts: 1000, IsTruncated: false,
		Part:              []PartEntry{{PartNumber: 1, LastModified: "2024-01-01T00:00:00.000Z", ETag: `"p1"`, Size: 5242880}, {PartNumber: 2, LastModified: "2024-01-01T00:00:00.000Z", ETag: `"p2"`, Size: 10}},
		ChecksumAlgorithm: "CRC32", ChecksumType: "FULL_OBJECT"})
	assertRoot(t, doc, "ListPartsResult")
	assertOrder(t, doc, "<Bucket>b</Bucket>", "<Key>k</Key>", "<UploadId>u</UploadId>", "<Initiator>", "<ID>i</ID>", "</Initiator>", "<Owner>", "<ID>o</ID>", "</Owner>",
		"<StorageClass>STANDARD</StorageClass>", "<PartNumberMarker>0</PartNumberMarker>", "<NextPartNumberMarker>2</NextPartNumberMarker>", "<MaxParts>1000</MaxParts>", "<IsTruncated>false</IsTruncated>",
		"<Part>", "<PartNumber>1</PartNumber>", "<Size>5242880</Size>", "</Part>", "<Part>", "<PartNumber>2</PartNumber>", "</Part>",
		"<ChecksumAlgorithm>CRC32</ChecksumAlgorithm>", "<ChecksumType>FULL_OBJECT</ChecksumType>")

	doc = mustMarshal(t, ListMultipartUploadsResult{Bucket: "b", KeyMarker: "", UploadIDMarker: "", NextKeyMarker: "nk", NextUploadIDMarker: "nu",
		Prefix: "p", Delimiter: "/", MaxUploads: 1000, IsTruncated: true,
		Upload:         []UploadEntry{{Key: "k", UploadID: "u", Initiator: &Owner{ID: "i"}, Owner: &Owner{ID: "o"}, StorageClass: "STANDARD", Initiated: "2024-01-01T00:00:00.000Z"}},
		CommonPrefixes: []CommonPrefix{{Prefix: "p/x/"}}})
	assertRoot(t, doc, "ListMultipartUploadsResult")
	assertOrder(t, doc, "<Bucket>b</Bucket>", "<KeyMarker></KeyMarker>", "<UploadIdMarker></UploadIdMarker>", "<NextKeyMarker>nk</NextKeyMarker>", "<NextUploadIdMarker>nu</NextUploadIdMarker>",
		"<Prefix>p</Prefix>", "<Delimiter>/</Delimiter>", "<MaxUploads>1000</MaxUploads>", "<IsTruncated>true</IsTruncated>",
		"<Upload>", "<Key>k</Key>", "<UploadId>u</UploadId>", "<Initiator>", "<Owner>", "<StorageClass>STANDARD</StorageClass>", "<Initiated>", "</Upload>", "<CommonPrefixes>")
}

func TestObjectResponses(t *testing.T) {
	doc := mustMarshal(t, CopyObjectResult{LastModified: "2024-01-01T00:00:00.000Z", ETag: `"e"`, Checksums: Checksums{ChecksumSHA256: "s="}})
	assertRoot(t, doc, "CopyObjectResult")
	assertOrder(t, doc, "<LastModified>2024-01-01T00:00:00.000Z</LastModified>", "<ETag>", "<ChecksumSHA256>s=</ChecksumSHA256>")

	doc = mustMarshal(t, CopyPartResult{LastModified: "2024-01-01T00:00:00.000Z", ETag: `"e"`})
	assertRoot(t, doc, "CopyPartResult")
	assertOrder(t, doc, "<LastModified>", "<ETag>")

	doc = mustMarshal(t, DeleteResult{
		Deleted: []DeletedObject{{Key: "a"}, {Key: "b", VersionID: "v", DeleteMarker: true, DeleteMarkerVersionID: "dm"}},
		Error:   []DeleteError{{Key: "c", Code: "AccessDenied", Message: "Access Denied"}},
	})
	assertRoot(t, doc, "DeleteResult")
	assertOrder(t, doc, "<Deleted>", "<Key>a</Key>", "</Deleted>", "<Deleted>", "<Key>b</Key>", "<VersionId>v</VersionId>", "<DeleteMarker>true</DeleteMarker>", "<DeleteMarkerVersionId>dm</DeleteMarkerVersionId>", "</Deleted>",
		"<Error>", "<Key>c</Key>", "<Code>AccessDenied</Code>", "<Message>Access Denied</Message>", "</Error>")
	if strings.Count(doc, "<DeleteMarker>") != 1 {
		t.Fatal("DeleteMarker=false must be omitted")
	}

	doc = mustMarshal(t, PostResponse{Location: "http://h/b/k", Bucket: "b", Key: "k", ETag: `"e"`})
	assertRoot(t, doc, "PostResponse")
	assertOrder(t, doc, "<Location>", "<Bucket>b</Bucket>", "<Key>k</Key>", "<ETag>")
}

func TestTaggingAndObjectLock(t *testing.T) {
	doc := mustMarshal(t, Tagging{TagSet: []Tag{{Key: "env", Value: "prod"}, {Key: "team", Value: "core"}}})
	assertRoot(t, doc, "Tagging")
	assertOrder(t, doc, "<TagSet>", "<Tag>", "<Key>env</Key>", "<Value>prod</Value>", "</Tag>", "<Tag>", "<Key>team</Key>", "</TagSet>")

	doc = mustMarshal(t, ObjectLockConfiguration{ObjectLockEnabled: "Enabled", Rule: &ObjectLockRule{DefaultRetention: DefaultRetention{Mode: "GOVERNANCE", Days: intp(30)}}})
	assertRoot(t, doc, "ObjectLockConfiguration")
	assertOrder(t, doc, "<ObjectLockEnabled>Enabled</ObjectLockEnabled>", "<Rule>", "<DefaultRetention>", "<Mode>GOVERNANCE</Mode>", "<Days>30</Days>", "</DefaultRetention>", "</Rule>")
	if strings.Contains(doc, "<Years>") {
		t.Fatal("Years must be omitted when nil")
	}

	doc = mustMarshal(t, Retention{Mode: "COMPLIANCE", RetainUntilDate: "2030-01-01T00:00:00.000Z"})
	assertRoot(t, doc, "Retention")
	assertOrder(t, doc, "<Mode>COMPLIANCE</Mode>", "<RetainUntilDate>2030-01-01T00:00:00.000Z</RetainUntilDate>")

	doc = mustMarshal(t, LegalHold{Status: "ON"})
	assertRoot(t, doc, "LegalHold")
	assertOrder(t, doc, "<Status>ON</Status>")
}

func TestFullControlACL(t *testing.T) {
	owner := Owner{ID: "75aa57f09aa0c8caeab4f8c24e99d10f8e7faeebf76c078efc7c6caea54ba06a", DisplayName: "bara"}
	acl := FullControlACL(owner)
	doc := mustMarshal(t, acl)
	assertRoot(t, doc, "AccessControlPolicy")
	assertOrder(t, doc, "<Owner>", "<ID>"+owner.ID+"</ID>", "<DisplayName>bara</DisplayName>", "</Owner>",
		"<AccessControlList>", "<Grant>",
		`<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser">`,
		"<ID>"+owner.ID+"</ID>", "<DisplayName>bara</DisplayName>", "</Grantee>",
		"<Permission>FULL_CONTROL</Permission>", "</Grant>", "</AccessControlList>")

	// Round trip: xsi:type must survive decoding.
	var back AccessControlPolicy
	if err := Unmarshal(strings.NewReader(doc), &back, 0); err != nil {
		t.Fatalf("Unmarshal ACL: %v", err)
	}
	if len(back.AccessControlList) != 1 {
		t.Fatalf("grants = %d", len(back.AccessControlList))
	}
	g := back.AccessControlList[0]
	if g.Grantee.Type != GranteeCanonicalUser || g.Grantee.ID != owner.ID || g.Permission != PermissionFullControl {
		t.Fatalf("round trip grant = %+v", g)
	}
	if back.Owner != owner || back.Xmlns != NS {
		t.Fatalf("round trip owner/xmlns = %+v / %q", back.Owner, back.Xmlns)
	}

	// A Group grantee as sent by AWS CLI (prefix declared on the root).
	body := `<AccessControlPolicy xmlns="http://s3.amazonaws.com/doc/2006-03-01/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <Owner><ID>o</ID></Owner>
  <AccessControlList>
    <Grant>
      <Grantee xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee>
      <Permission>READ</Permission>
    </Grant>
  </AccessControlList>
</AccessControlPolicy>`
	back = AccessControlPolicy{}
	if err := Unmarshal(strings.NewReader(body), &back, 0); err != nil {
		t.Fatalf("Unmarshal group ACL: %v", err)
	}
	if back.AccessControlList[0].Grantee.Type != GranteeGroup || back.AccessControlList[0].Grantee.URI == "" {
		t.Fatalf("group grantee = %+v", back.AccessControlList[0].Grantee)
	}

	// Decoding a malformed Grantee surfaces the decoder error.
	var g2 Grantee
	if err := xml.Unmarshal([]byte(`<Grantee><ID>unterminated</Grantee>`), &g2); err == nil {
		t.Fatal("expected decode error for malformed grantee")
	}
}

func TestUnmarshalCompleteMultipartUpload(t *testing.T) {
	body := `<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
   <Part>
      <ETag>"a54357aff0632cce46d942af68356b38"</ETag>
      <PartNumber>1</PartNumber>
      <ChecksumCRC32C>sOO8/Q==</ChecksumCRC32C>
   </Part>
   <Part>
      <ETag>"0c78aef83f66abc1fa1e8477f296d394"</ETag>
      <PartNumber>2</PartNumber>
   </Part>
</CompleteMultipartUpload>`
	var req CompleteMultipartUpload
	if err := Unmarshal(strings.NewReader(body), &req, 0); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.Xmlns != NS || len(req.Part) != 2 {
		t.Fatalf("parsed = %+v", req)
	}
	if req.Part[0].PartNumber != 1 || req.Part[0].ETag != `"a54357aff0632cce46d942af68356b38"` || req.Part[0].ChecksumCRC32C != "sOO8/Q==" {
		t.Fatalf("part 1 = %+v", req.Part[0])
	}
	if req.Part[1].PartNumber != 2 || req.Part[1].ChecksumCRC32C != "" {
		t.Fatalf("part 2 = %+v", req.Part[1])
	}
	// Without namespace is accepted too.
	if err := Unmarshal(strings.NewReader(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"x"</ETag></Part></CompleteMultipartUpload>`), &req, 0); err != nil {
		t.Fatalf("no-namespace body rejected: %v", err)
	}
}

func TestUnmarshalDelete(t *testing.T) {
	body := `<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
   <Quiet>true</Quiet>
   <Object>
      <Key>sample1.txt</Key>
   </Object>
   <Object>
      <Key>sample2.txt</Key>
      <VersionId>OYcLXagmS.WaD..oyH4KRguB95_YhLs7</VersionId>
   </Object>
</Delete>`
	var req Delete
	if err := Unmarshal(strings.NewReader(body), &req, 0); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !req.Quiet || len(req.Object) != 2 || req.Object[0].Key != "sample1.txt" || req.Object[1].VersionID != "OYcLXagmS.WaD..oyH4KRguB95_YhLs7" {
		t.Fatalf("parsed = %+v", req)
	}
}

func TestUnmarshalCORS(t *testing.T) {
	body := `<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
 <CORSRule>
   <ID>rule1</ID>
   <AllowedOrigin>http://www.example.com</AllowedOrigin>
   <AllowedMethod>PUT</AllowedMethod>
   <AllowedMethod>POST</AllowedMethod>
   <AllowedMethod>DELETE</AllowedMethod>
   <AllowedHeader>*</AllowedHeader>
   <MaxAgeSeconds>3000</MaxAgeSeconds>
   <ExposeHeader>x-amz-server-side-encryption</ExposeHeader>
 </CORSRule>
 <CORSRule>
   <AllowedOrigin>*</AllowedOrigin>
   <AllowedMethod>GET</AllowedMethod>
 </CORSRule>
</CORSConfiguration>`
	var cfg CORSConfiguration
	if err := Unmarshal(strings.NewReader(body), &cfg, 0); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(cfg.CORSRule) != 2 {
		t.Fatalf("rules = %d", len(cfg.CORSRule))
	}
	r := cfg.CORSRule[0]
	if r.ID != "rule1" || len(r.AllowedMethod) != 3 || r.AllowedMethod[2] != "DELETE" || r.AllowedHeader[0] != "*" ||
		r.MaxAgeSeconds == nil || *r.MaxAgeSeconds != 3000 || r.ExposeHeader[0] != "x-amz-server-side-encryption" {
		t.Fatalf("rule 1 = %+v", r)
	}
	if cfg.CORSRule[1].MaxAgeSeconds != nil || cfg.CORSRule[1].ID != "" {
		t.Fatalf("rule 2 = %+v", cfg.CORSRule[1])
	}
	// Echo back with namespace, ID omitted on second rule.
	doc := mustMarshal(t, cfg)
	assertRoot(t, doc, "CORSConfiguration")
	assertOrder(t, doc, "<CORSRule>", "<ID>rule1</ID>", "<AllowedOrigin>http://www.example.com</AllowedOrigin>", "<AllowedMethod>PUT</AllowedMethod>", "<MaxAgeSeconds>3000</MaxAgeSeconds>", "</CORSRule>", "<CORSRule>", "<AllowedOrigin>*</AllowedOrigin>")
	if strings.Count(doc, "<ID>") != 1 || strings.Count(doc, "<MaxAgeSeconds>") != 1 {
		t.Fatalf("omitempty not honoured:\n%s", doc)
	}
}

func TestUnmarshalLifecycle(t *testing.T) {
	body := `<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Rule>
    <ID>Archive and then delete rule</ID>
    <Filter>
      <And>
        <Prefix>projectdocs/</Prefix>
        <Tag>
          <Key>key1</Key>
          <Value>value1</Value>
        </Tag>
        <Tag>
          <Key>key2</Key>
          <Value>value2</Value>
        </Tag>
        <ObjectSizeGreaterThan>500</ObjectSizeGreaterThan>
        <ObjectSizeLessThan>64000</ObjectSizeLessThan>
      </And>
    </Filter>
    <Status>Enabled</Status>
    <Expiration>
      <Days>365</Days>
    </Expiration>
    <NoncurrentVersionExpiration>
      <NoncurrentDays>30</NoncurrentDays>
      <NewerNoncurrentVersions>2</NewerNoncurrentVersions>
    </NoncurrentVersionExpiration>
    <AbortIncompleteMultipartUpload>
      <DaysAfterInitiation>7</DaysAfterInitiation>
    </AbortIncompleteMultipartUpload>
  </Rule>
  <Rule>
    <ID>expire-markers</ID>
    <Filter>
      <Prefix>logs/</Prefix>
    </Filter>
    <Status>Disabled</Status>
    <Expiration>
      <ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker>
    </Expiration>
  </Rule>
  <Rule>
    <ID>legacy</ID>
    <Prefix>old/</Prefix>
    <Status>Enabled</Status>
    <Expiration>
      <Date>2030-01-01T00:00:00.000Z</Date>
    </Expiration>
  </Rule>
</LifecycleConfiguration>`
	var cfg LifecycleConfiguration
	if err := Unmarshal(strings.NewReader(body), &cfg, 0); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(cfg.Rule) != 3 {
		t.Fatalf("rules = %d", len(cfg.Rule))
	}
	r := cfg.Rule[0]
	if r.ID != "Archive and then delete rule" || r.Status != "Enabled" || r.Filter == nil || r.Filter.And == nil {
		t.Fatalf("rule 0 = %+v", r)
	}
	and := r.Filter.And
	if and.Prefix != "projectdocs/" || len(and.Tag) != 2 || and.Tag[1].Value != "value2" ||
		and.ObjectSizeGreaterThan == nil || *and.ObjectSizeGreaterThan != 500 || and.ObjectSizeLessThan == nil || *and.ObjectSizeLessThan != 64000 {
		t.Fatalf("filter.and = %+v", and)
	}
	if r.Expiration == nil || r.Expiration.Days == nil || *r.Expiration.Days != 365 || r.Expiration.Date != "" || r.Expiration.ExpiredObjectDeleteMarker != nil {
		t.Fatalf("expiration = %+v", r.Expiration)
	}
	if r.NoncurrentVersionExpiration == nil || *r.NoncurrentVersionExpiration.NoncurrentDays != 30 || *r.NoncurrentVersionExpiration.NewerNoncurrentVersions != 2 {
		t.Fatalf("ncve = %+v", r.NoncurrentVersionExpiration)
	}
	if r.AbortIncompleteMultipartUpload == nil || *r.AbortIncompleteMultipartUpload.DaysAfterInitiation != 7 {
		t.Fatalf("abort = %+v", r.AbortIncompleteMultipartUpload)
	}
	r = cfg.Rule[1]
	if r.Filter == nil || r.Filter.Prefix != "logs/" || r.Filter.And != nil || r.Filter.Tag != nil || r.Expiration.ExpiredObjectDeleteMarker == nil || !*r.Expiration.ExpiredObjectDeleteMarker {
		t.Fatalf("rule 1 = %+v", r)
	}
	r = cfg.Rule[2]
	if r.Filter != nil || r.Prefix != "old/" || r.Expiration.Date != "2030-01-01T00:00:00.000Z" || r.Expiration.Days != nil {
		t.Fatalf("rule 2 = %+v", r)
	}

	// Echo preserves structure.
	doc := mustMarshal(t, cfg)
	assertRoot(t, doc, "LifecycleConfiguration")
	assertOrder(t, doc, "<Rule>", "<ID>Archive and then delete rule</ID>", "<Status>Enabled</Status>", "<Filter>", "<And>", "<Prefix>projectdocs/</Prefix>", "<Tag>", "<ObjectSizeGreaterThan>500</ObjectSizeGreaterThan>", "</And>", "</Filter>",
		"<Expiration>", "<Days>365</Days>", "</Expiration>", "<NoncurrentVersionExpiration>", "<AbortIncompleteMultipartUpload>", "</Rule>")
	// Rule with Filter.Tag renders correctly.
	doc = mustMarshal(t, LifecycleConfiguration{Rule: []LifecycleRule{{ID: "t", Status: "Enabled", Filter: &LifecycleFilter{Tag: &Tag{Key: "k", Value: "v"}, ObjectSizeGreaterThan: int64p(1)}, Expiration: &Expiration{Days: intp(1)}}}})
	assertOrder(t, doc, "<Filter>", "<Tag>", "<Key>k</Key>", "</Tag>", "<ObjectSizeGreaterThan>1</ObjectSizeGreaterThan>", "</Filter>")
}

func TestUnmarshalVersioningAndCreateBucket(t *testing.T) {
	var v VersioningConfiguration
	body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Status>Enabled</Status>
</VersioningConfiguration>`
	if err := Unmarshal(strings.NewReader(body), &v, 0); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if v.Status != VersioningEnabled || v.MfaDelete != "" {
		t.Fatalf("versioning = %+v", v)
	}
	v = VersioningConfiguration{}
	if err := Unmarshal(strings.NewReader(`<VersioningConfiguration><Status>Suspended</Status><MfaDelete>Disabled</MfaDelete></VersioningConfiguration>`), &v, 0); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if v.Status != VersioningSuspended || v.MfaDelete != "Disabled" {
		t.Fatalf("versioning = %+v", v)
	}

	var c CreateBucketConfiguration
	body = `<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
   <LocationConstraint>eu-west-1</LocationConstraint>
</CreateBucketConfiguration>`
	if err := Unmarshal(strings.NewReader(body), &c, 0); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if c.LocationConstraint != "eu-west-1" {
		t.Fatalf("create bucket = %+v", c)
	}
	// Empty location (us-east-1) is fine.
	c = CreateBucketConfiguration{}
	if err := Unmarshal(strings.NewReader(`<CreateBucketConfiguration/>`), &c, 0); err != nil || c.LocationConstraint != "" {
		t.Fatalf("empty create bucket: %+v, %v", c, err)
	}

	// Tagging and ObjectLock request bodies round-trip too.
	var tg Tagging
	if err := Unmarshal(strings.NewReader(`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag></TagSet></Tagging>`), &tg, 0); err != nil || len(tg.TagSet) != 1 || tg.TagSet[0].Key != "a" {
		t.Fatalf("tagging: %+v, %v", tg, err)
	}
	var ol ObjectLockConfiguration
	if err := Unmarshal(strings.NewReader(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>`), &ol, 0); err != nil || ol.Rule == nil || ol.Rule.DefaultRetention.Years == nil || *ol.Rule.DefaultRetention.Years != 1 || ol.Rule.DefaultRetention.Days != nil {
		t.Fatalf("object lock: %+v, %v", ol, err)
	}
}

func TestUnmarshalErrors(t *testing.T) {
	var v VersioningConfiguration

	// Wrong root element.
	err := Unmarshal(strings.NewReader(`<Tagging/>`), &v, 0)
	if !errors.Is(err, s3err.New(s3err.MalformedXML)) {
		t.Fatalf("wrong root: %v", err)
	}
	// Not well formed.
	err = Unmarshal(strings.NewReader(`<VersioningConfiguration><Status>Enabled</VersioningConfiguration>`), &v, 0)
	var se *s3err.Error
	if !errors.As(err, &se) || se.Code != s3err.MalformedXML || se.HTTPStatus != 400 {
		t.Fatalf("malformed: %v", err)
	}
	if !strings.Contains(se.Message, "not well formed") {
		t.Fatalf("message should keep the AWS default prefix: %q", se.Message)
	}
	// Trailing garbage after the root.
	err = Unmarshal(strings.NewReader(`<VersioningConfiguration/><VersioningConfiguration/>`), &v, 0)
	if !errors.Is(err, s3err.New(s3err.MalformedXML)) {
		t.Fatalf("trailing content: %v", err)
	}
	// Trailing whitespace and comments are fine.
	if err := Unmarshal(strings.NewReader("<VersioningConfiguration/>\n\n <!-- done --> "), &v, 0); err != nil {
		t.Fatalf("trailing whitespace: %v", err)
	}
	// Trailing text is not.
	err = Unmarshal(strings.NewReader("<VersioningConfiguration/>junk"), &v, 0)
	if !errors.Is(err, s3err.New(s3err.MalformedXML)) {
		t.Fatalf("trailing text: %v", err)
	}
	// Trailing malformed markup is not.
	err = Unmarshal(strings.NewReader("<VersioningConfiguration/><oops"), &v, 0)
	if !errors.Is(err, s3err.New(s3err.MalformedXML)) {
		t.Fatalf("trailing malformed: %v", err)
	}
	// Empty body.
	err = Unmarshal(strings.NewReader("  \n"), &v, 0)
	if !errors.Is(err, s3err.New(s3err.MissingRequestBodyError)) {
		t.Fatalf("empty: %v", err)
	}
	// Reader failure.
	err = Unmarshal(io.MultiReader(strings.NewReader("<Versioning"), errReader{}), &v, 0)
	if !errors.Is(err, s3err.New(s3err.IncompleteBody)) {
		t.Fatalf("read error: %v", err)
	}
}

func TestUnmarshalSizeLimit(t *testing.T) {
	var v VersioningConfiguration
	body := `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`

	// Exactly at the limit is accepted.
	if err := Unmarshal(strings.NewReader(body), &v, int64(len(body))); err != nil {
		t.Fatalf("at limit: %v", err)
	}
	// One byte over is rejected with MaxMessageLengthExceeded.
	err := Unmarshal(strings.NewReader(body), &v, int64(len(body))-1)
	if !errors.Is(err, s3err.New(s3err.MaxMessageLengthExceeded)) {
		t.Fatalf("over limit: %v", err)
	}
	var se *s3err.Error
	if !errors.As(err, &se) || se.HTTPStatus != 400 {
		t.Fatalf("over limit status: %v", err)
	}
	// Default limit applies when limit <= 0: 1 MiB + 1 of padding is rejected
	// and the reader is not drained further.
	big := io.MultiReader(strings.NewReader("<VersioningConfiguration>"), bytes.NewReader(bytes.Repeat([]byte(" "), int(DefaultLimit))), strings.NewReader("</VersioningConfiguration>"))
	if err := Unmarshal(big, &v, -1); !errors.Is(err, s3err.New(s3err.MaxMessageLengthExceeded)) {
		t.Fatalf("default limit: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func strPtr(s string) *string { return &s }
