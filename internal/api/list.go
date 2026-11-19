package api

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/enetforge/hogshead/internal/api/s3err"
	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/meta"
)

const maxListKeys = 1000

// encodeKey applies encoding-type=url rules: everything except unreserved
// characters and '/' is percent-encoded.
func encodeKey(s string, enabled bool) string {
	if !enabled {
		return s
	}
	var sb strings.Builder
	for _, b := range []byte(s) {
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-', b == '_', b == '.', b == '~', b == '/':
			sb.WriteByte(b)
		default:
			sb.WriteString("%" + strings.ToUpper(hexByte(b)))
		}
	}
	return sb.String()
}

func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0xf]})
}

func encodingType(q url.Values) (bool, error) {
	switch q.Get("encoding-type") {
	case "":
		return false, nil
	case "url":
		return true, nil
	}
	return false, s3err.New(s3err.InvalidArgument).WithMessage("Invalid Encoding Method specified in Request")
}

func encodeToken(cursor string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor))
}

func decodeToken(tok string) (string, error) {
	if tok == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", s3err.New(s3err.InvalidArgument).WithMessage("The continuation token provided is incorrect")
	}
	return string(b), nil
}

func objectEntry(v *meta.ObjectVersion, owner *s3xml.Owner, enc bool) s3xml.Object {
	o := s3xml.Object{
		Key:          encodeKey(v.Key, enc),
		LastModified: s3xml.FormatTime(v.Created),
		ETag:         QuoteETag(v.ETag),
		Size:         v.Size,
		StorageClass: storageClassOf(v.StorageClass),
		Owner:        owner,
	}
	if algo := checksumAlgoOf(v.Checksums); algo != "" {
		o.ChecksumAlgorithm = []string{algo}
		o.ChecksumType = v.Checksums.Type
	}
	return o
}

func storageClassOf(s string) string {
	if s == "" {
		return "STANDARD"
	}
	return s
}

func checksumAlgoOf(c meta.Checksums) string {
	switch {
	case c.CRC64NVME != "":
		return "CRC64NVME"
	case c.CRC32C != "":
		return "CRC32C"
	case c.CRC32 != "":
		return "CRC32"
	case c.SHA256 != "":
		return "SHA256"
	case c.SHA1 != "":
		return "SHA1"
	}
	return ""
}

func (h *Handler) listObjectsV1(c *reqCtx) error {
	q := c.r.URL.Query()
	enc, err := encodingType(q)
	if err != nil {
		return err
	}
	max, err := ParseInt(q.Get("max-keys"), maxListKeys, 0, maxListKeys)
	if err != nil {
		return s3err.New(s3err.InvalidArgument).WithMessage("max-keys must be a non-negative integer")
	}
	opts := meta.ListOpts{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), StartAfter: q.Get("marker"), Max: max}
	res := &meta.ListResult{}
	if max > 0 {
		if res, err = h.d.DB.ListObjects(c.ctx(), c.bucket.Name, opts); err != nil {
			return err
		}
	}
	marker := encodeKey(opts.StartAfter, enc)
	out := s3xml.ListBucketResult{
		Name: c.bucket.Name, Prefix: encodeKey(opts.Prefix, enc), Marker: &marker,
		MaxKeys: max, Delimiter: encodeKey(opts.Delimiter, enc), IsTruncated: res.IsTruncated,
		Contents: []s3xml.Object{}, CommonPrefixes: []s3xml.CommonPrefix{},
	}
	if enc {
		out.EncodingType = "url"
	}
	if res.IsTruncated && opts.Delimiter != "" {
		out.NextMarker = encodeKey(res.NextCursor, enc)
	}
	owners := map[string]*s3xml.Owner{}
	for i := range res.Objects {
		v := &res.Objects[i]
		ow, ok := owners[v.Owner]
		if !ok {
			ow = h.owner(c, v.Owner)
			owners[v.Owner] = ow
		}
		out.Contents = append(out.Contents, objectEntry(v, ow, enc))
	}
	for _, p := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, s3xml.CommonPrefix{Prefix: encodeKey(p, enc)})
	}
	return c.xml(http.StatusOK, &out)
}

func (h *Handler) listObjectsV2(c *reqCtx) error {
	q := c.r.URL.Query()
	enc, err := encodingType(q)
	if err != nil {
		return err
	}
	max, err := ParseInt(q.Get("max-keys"), maxListKeys, 0, maxListKeys)
	if err != nil {
		return s3err.New(s3err.InvalidArgument).WithMessage("max-keys must be a non-negative integer")
	}
	cursor, err := decodeToken(q.Get("continuation-token"))
	if err != nil {
		return err
	}
	startAfter := q.Get("start-after")
	if cursor == "" {
		cursor = startAfter
	}
	opts := meta.ListOpts{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), StartAfter: cursor, Max: max}
	res := &meta.ListResult{}
	if max > 0 {
		if res, err = h.d.DB.ListObjects(c.ctx(), c.bucket.Name, opts); err != nil {
			return err
		}
	}
	count := len(res.Objects) + len(res.CommonPrefixes)
	out := s3xml.ListBucketResult{
		Name: c.bucket.Name, Prefix: encodeKey(opts.Prefix, enc), StartAfter: encodeKey(startAfter, enc),
		ContinuationToken: q.Get("continuation-token"), KeyCount: &count,
		MaxKeys: max, Delimiter: encodeKey(opts.Delimiter, enc), IsTruncated: res.IsTruncated,
		Contents: []s3xml.Object{}, CommonPrefixes: []s3xml.CommonPrefix{},
	}
	if enc {
		out.EncodingType = "url"
	}
	if res.IsTruncated {
		out.NextContinuationToken = encodeToken(res.NextCursor)
	}
	fetchOwner := q.Get("fetch-owner") == "true"
	owners := map[string]*s3xml.Owner{}
	for i := range res.Objects {
		v := &res.Objects[i]
		var ow *s3xml.Owner
		if fetchOwner {
			var ok bool
			if ow, ok = owners[v.Owner]; !ok {
				ow = h.owner(c, v.Owner)
				owners[v.Owner] = ow
			}
		}
		out.Contents = append(out.Contents, objectEntry(v, ow, enc))
	}
	for _, p := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, s3xml.CommonPrefix{Prefix: encodeKey(p, enc)})
	}
	return c.xml(http.StatusOK, &out)
}

func (h *Handler) listObjectVersions(c *reqCtx) error {
	q := c.r.URL.Query()
	enc, err := encodingType(q)
	if err != nil {
		return err
	}
	max, err := ParseInt(q.Get("max-keys"), maxListKeys, 0, maxListKeys)
	if err != nil {
		return s3err.New(s3err.InvalidArgument).WithMessage("max-keys must be a non-negative integer")
	}
	opts := meta.VersionListOpts{
		Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"),
		KeyMarker: q.Get("key-marker"), VersionIDMarker: q.Get("version-id-marker"), Max: max,
	}
	res := &meta.VersionListResult{}
	if max > 0 {
		if res, err = h.d.DB.ListVersions(c.ctx(), c.bucket.Name, opts); err != nil {
			return err
		}
	}
	out := s3xml.ListVersionsResult{
		Name: c.bucket.Name, Prefix: encodeKey(opts.Prefix, enc), KeyMarker: encodeKey(opts.KeyMarker, enc),
		VersionIDMarker: opts.VersionIDMarker, MaxKeys: max, Delimiter: encodeKey(opts.Delimiter, enc),
		IsTruncated: res.IsTruncated, Version: []s3xml.ObjectVersion{}, DeleteMarker: []s3xml.DeleteMarkerEntry{},
		CommonPrefixes: []s3xml.CommonPrefix{},
	}
	if enc {
		out.EncodingType = "url"
	}
	if res.IsTruncated {
		out.NextKeyMarker = encodeKey(res.NextKeyMarker, enc)
		out.NextVersionIDMarker = res.NextVersionIDMarker
	}
	owners := map[string]*s3xml.Owner{}
	for i := range res.Versions {
		v := &res.Versions[i]
		ow, ok := owners[v.Owner]
		if !ok {
			ow = h.owner(c, v.Owner)
			owners[v.Owner] = ow
		}
		if v.DeleteMarker {
			out.DeleteMarker = append(out.DeleteMarker, s3xml.DeleteMarkerEntry{
				Key: encodeKey(v.Key, enc), VersionID: v.VersionID, IsLatest: v.IsLatest,
				LastModified: s3xml.FormatTime(v.Created), Owner: ow,
			})
			continue
		}
		e := s3xml.ObjectVersion{
			Key: encodeKey(v.Key, enc), VersionID: v.VersionID, IsLatest: v.IsLatest,
			LastModified: s3xml.FormatTime(v.Created), ETag: QuoteETag(v.ETag), Size: v.Size,
			StorageClass: storageClassOf(v.StorageClass), Owner: ow,
		}
		if algo := checksumAlgoOf(v.Checksums); algo != "" {
			e.ChecksumAlgorithm = []string{algo}
		}
		out.Version = append(out.Version, e)
	}
	for _, p := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, s3xml.CommonPrefix{Prefix: encodeKey(p, enc)})
	}
	return c.xml(http.StatusOK, &out)
}

func (h *Handler) listMultipartUploads(c *reqCtx) error {
	q := c.r.URL.Query()
	enc, err := encodingType(q)
	if err != nil {
		return err
	}
	max, err := ParseInt(q.Get("max-uploads"), maxListKeys, 0, maxListKeys)
	if err != nil {
		return s3err.New(s3err.InvalidArgument).WithMessage("max-uploads must be a non-negative integer")
	}
	opts := meta.UploadListOpts{
		Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"),
		KeyMarker: q.Get("key-marker"), UploadIDMarker: q.Get("upload-id-marker"), Max: max,
	}
	res := &meta.UploadListResult{}
	if max > 0 {
		if res, err = h.d.DB.ListUploads(c.ctx(), c.bucket.Name, opts); err != nil {
			return err
		}
	}
	out := s3xml.ListMultipartUploadsResult{
		Bucket: c.bucket.Name, KeyMarker: encodeKey(opts.KeyMarker, enc), UploadIDMarker: opts.UploadIDMarker,
		Prefix: encodeKey(opts.Prefix, enc), Delimiter: encodeKey(opts.Delimiter, enc), MaxUploads: max,
		IsTruncated: res.IsTruncated, Upload: []s3xml.UploadEntry{}, CommonPrefixes: []s3xml.CommonPrefix{},
	}
	if res.IsTruncated {
		out.NextKeyMarker = encodeKey(res.NextKeyMarker, enc)
		out.NextUploadIDMarker = res.NextUploadIDMarker
	}
	for _, u := range res.Uploads {
		ow := h.owner(c, u.Owner)
		out.Upload = append(out.Upload, s3xml.UploadEntry{
			Key: encodeKey(u.Key, enc), UploadID: u.ID, Initiator: ow, Owner: ow,
			StorageClass: storageClassOf(u.StorageClass), Initiated: s3xml.FormatTime(u.Created),
		})
	}
	for _, p := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, s3xml.CommonPrefix{Prefix: encodeKey(p, enc)})
	}
	return c.xml(http.StatusOK, &out)
}
