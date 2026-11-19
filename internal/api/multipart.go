package api

import (
	"crypto/md5"  //nolint:gosec // S3 multipart ETags are MD5 by protocol
	"crypto/sha1" //nolint:gosec
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strconv"
	"strings"

	"github.com/enetforge/hogshead/internal/api/s3err"
	"github.com/enetforge/hogshead/internal/api/s3xml"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/store"
	"github.com/enetforge/hogshead/internal/store/crc64nvme"
	"github.com/enetforge/hogshead/internal/store/crccombine"
)

func (h *Handler) createMultipartUpload(c *reqCtx) error {
	m, err := h.objectMetadataFromRequest(c)
	if err != nil {
		return err
	}
	algo := normalizeAlgo(c.r.Header.Get("x-amz-checksum-algorithm"))
	if c.r.Header.Get("x-amz-checksum-algorithm") != "" && algo == "" {
		return s3err.New(s3err.InvalidRequest).WithMessage("Unsupported checksum algorithm")
	}
	ctype := strings.ToUpper(c.r.Header.Get("x-amz-checksum-type"))
	switch ctype {
	case "":
		if algo == "CRC64NVME" {
			ctype = "FULL_OBJECT"
		} else if algo != "" {
			ctype = "COMPOSITE"
		}
	case "COMPOSITE":
		if algo == "CRC64NVME" {
			return s3err.New(s3err.InvalidRequest).WithMessage("CRC64NVME only supports FULL_OBJECT checksums")
		}
	case "FULL_OBJECT":
		if algo == "SHA1" || algo == "SHA256" {
			return s3err.New(s3err.InvalidRequest).WithMessage("SHA checksums only support COMPOSITE for multipart uploads")
		}
	default:
		return s3err.New(s3err.InvalidRequest).WithMessage("Unknown checksum type")
	}
	cmd := &meta.CreateUpload{
		UploadID: ids.UploadID(), Bucket: c.bucket.Name, Key: c.target.Key, Owner: h.callerID(c),
		ContentType: m["content-type"], Metadata: m, ChecksumAlgo: algo, ChecksumType: ctype,
		StorageClass: c.r.Header.Get("x-amz-storage-class"), Now: h.nowMs(),
	}
	if _, err := h.propose(c, cmd); err != nil {
		return err
	}
	if algo != "" {
		c.w.Header().Set("x-amz-checksum-algorithm", algo)
		c.w.Header().Set("x-amz-checksum-type", ctype)
	}
	return c.xml(http.StatusOK, &s3xml.InitiateMultipartUploadResult{Bucket: c.bucket.Name, Key: c.target.Key, UploadID: cmd.UploadID})
}

func (h *Handler) loadUpload(c *reqCtx) (*meta.Upload, error) {
	id := c.r.URL.Query().Get("uploadId")
	u, err := h.d.DB.GetUpload(c.ctx(), id)
	if err != nil || u.Bucket != c.bucket.Name || u.Key != c.target.Key {
		return nil, s3err.New(s3err.NoSuchUpload)
	}
	return u, nil
}

func partNumber(c *reqCtx) (int, error) {
	n, err := strconv.Atoi(c.r.URL.Query().Get("partNumber"))
	if err != nil || n < 1 || n > 10000 {
		return 0, s3err.New(s3err.InvalidArgument).WithMessage("Part number must be an integer between 1 and 10000, inclusive")
	}
	return n, nil
}

func (h *Handler) uploadPart(c *reqCtx) error {
	u, err := h.loadUpload(c)
	if err != nil {
		return err
	}
	n, err := partNumber(c)
	if err != nil {
		return err
	}
	res, err := h.ingest(c, []string{u.ChecksumAlgo}, h.d.Cfg().Limits.MaxPartSize)
	if err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.PutPart{UploadID: u.ID, Number: n, Size: res.Size, ETag: res.MD5, Chunks: res.Chunks, Checksums: res.Checksums, Now: h.nowMs()}); err != nil {
		return err
	}
	c.w.Header().Set("ETag", QuoteETag(res.MD5))
	setChecksumHeaders(c.w.Header(), res.Checksums)
	return c.empty(http.StatusOK)
}

func (h *Handler) uploadPartCopy(c *reqCtx) error {
	u, err := h.loadUpload(c)
	if err != nil {
		return err
	}
	n, err := partNumber(c)
	if err != nil {
		return err
	}
	srcBucket, srcKey, srcVid, err := CopySource(c.r.Header.Get("x-amz-copy-source"))
	if err != nil {
		return s3err.New(s3err.InvalidArgument).WithMessage("Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	src, err := h.loadSource(c, srcBucket, srcKey, srcVid)
	if err != nil {
		return err
	}
	ch := http.Header{}
	for _, k := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if v := c.r.Header.Get("x-amz-copy-source-" + strings.ToLower(k)); v != "" {
			ch.Set(k, v)
		}
	}
	if EvaluateConditions(ch, QuoteETag(src.ETag), src.Created) != CondOK {
		return s3err.New(s3err.PreconditionFailed)
	}
	off, length := int64(0), src.Size
	if rh := c.r.Header.Get("x-amz-copy-source-range"); rh != "" {
		o, l, ok, rerr := ParseRange(rh, src.Size)
		if rerr != nil || !ok {
			return s3err.New(s3err.InvalidArgument).WithMessage("The x-amz-copy-source-range value must be of the form bytes=first-last")
		}
		off, length = o, l
	}
	if length > h.d.Cfg().Limits.MaxPartSize {
		return s3err.New(s3err.EntityTooLarge)
	}
	rc, err := store.Open(c.ctx(), h.d.Chunks, src.Chunks, off, length)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	var algos []string
	if u.ChecksumAlgo != "" {
		algos = []string{u.ChecksumAlgo}
	}
	res, err := store.Write(c.ctx(), h.d.Chunks, rc, store.WriteOpts{Algorithms: algos})
	if err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.PutPart{UploadID: u.ID, Number: n, Size: res.Size, ETag: res.MD5, Chunks: res.Chunks, Checksums: res.Checksums, Now: h.nowMs()}); err != nil {
		return err
	}
	if src.VersionID != meta.NullVersionID {
		c.w.Header().Set("x-amz-copy-source-version-id", src.VersionID)
	}
	out := s3xml.CopyPartResult{LastModified: s3xml.FormatTime(h.now().UTC()), ETag: QuoteETag(res.MD5)}
	out.Checksums = xmlChecksums(res.Checksums)
	return c.xml(http.StatusOK, &out)
}

// multipartChecksum computes the object checksum from part checksums.
func multipartChecksum(algo, ctype string, parts []meta.Part) (string, error) {
	if algo == "" {
		return "", nil
	}
	if ctype == "FULL_OBJECT" {
		var poly crccombine.Poly
		var bits int
		switch algo {
		case "CRC32":
			poly, bits = crccombine.CRC32IEEE, 32
		case "CRC32C":
			poly, bits = crccombine.CRC32C, 32
		case "CRC64NVME":
			poly, bits = crccombine.CRC64NVME, 64
		default:
			return "", fmt.Errorf("algorithm %s does not support FULL_OBJECT", algo)
		}
		var acc uint64
		for i, p := range parts {
			raw, err := base64.StdEncoding.DecodeString(store.ChecksumValue(p.Checksums, algo))
			if err != nil || len(raw) != bits/8 {
				return "", fmt.Errorf("part %d is missing a %s checksum", p.Number, algo)
			}
			var v uint64
			if bits == 32 {
				v = uint64(binary.BigEndian.Uint32(raw))
			} else {
				v = binary.BigEndian.Uint64(raw)
			}
			if i == 0 {
				acc = v
			} else {
				acc = poly.Combine(acc, v, p.Size)
			}
		}
		out := make([]byte, bits/8)
		if bits == 32 {
			binary.BigEndian.PutUint32(out, uint32(acc))
		} else {
			binary.BigEndian.PutUint64(out, acc)
		}
		return base64.StdEncoding.EncodeToString(out), nil
	}
	// COMPOSITE: hash of the concatenated raw part checksums, suffixed with -N.
	var concat []byte
	for _, p := range parts {
		raw, err := base64.StdEncoding.DecodeString(store.ChecksumValue(p.Checksums, algo))
		if err != nil || len(raw) == 0 {
			return "", fmt.Errorf("part %d is missing a %s checksum", p.Number, algo)
		}
		concat = append(concat, raw...)
	}
	var sum []byte
	switch algo {
	case "CRC32":
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, crc32.ChecksumIEEE(concat))
		sum = b
	case "CRC32C":
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, crc32.Checksum(concat, crc32.MakeTable(crc32.Castagnoli)))
		sum = b
	case "SHA1":
		s := sha1.Sum(concat) //nolint:gosec
		sum = s[:]
	case "SHA256":
		s := sha256.Sum256(concat)
		sum = s[:]
	case "CRC64NVME":
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, crc64nvme.Checksum(concat))
		sum = b
	}
	return base64.StdEncoding.EncodeToString(sum) + "-" + strconv.Itoa(len(parts)), nil
}

func (h *Handler) completeMultipartUpload(c *reqCtx) error {
	u, err := h.loadUpload(c)
	if err != nil {
		// A retry of a completed upload gets the original answer.
		if prev, bucket, key, ok := h.d.DB.CompletedUploadResult(c.ctx(), c.r.URL.Query().Get("uploadId")); ok && bucket == c.bucket.Name && key == c.target.Key {
			return h.completeResponse(c, prev.ETag, prev.VersionID, meta.Checksums{})
		}
		return err
	}
	var req s3xml.CompleteMultipartUpload
	if err := parseXMLBody(c, &req); err != nil {
		return err
	}
	if len(req.Part) == 0 {
		return s3err.New(s3err.MalformedXML).WithMessage("The XML you provided was not well-formed or did not validate against our published schema")
	}
	stored, _, err := h.d.DB.ListParts(c.ctx(), u.ID, 0, 10000)
	if err != nil {
		return err
	}
	byNum := map[int]meta.Part{}
	for _, p := range stored {
		byNum[p.Number] = p
	}
	var parts []meta.Part
	var cparts []meta.CompletePart
	var md5s []byte
	var sizes []string
	last := 0
	for _, rp := range req.Part {
		if rp.PartNumber <= last {
			return s3err.New(s3err.InvalidPartOrder)
		}
		last = rp.PartNumber
		p, ok := byNum[rp.PartNumber]
		if !ok || !strings.EqualFold(normalizeETag(p.ETag), normalizeETag(rp.ETag)) {
			return s3err.Newf(s3err.InvalidPart, "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.")
		}
		if u.ChecksumAlgo != "" {
			want := store.ChecksumValue(p.Checksums, u.ChecksumAlgo)
			if got := partChecksumFromRequest(rp, u.ChecksumAlgo); got != "" && got != want {
				return s3err.New(s3err.InvalidPart).WithMessage("Part checksum does not match")
			}
		}
		raw, _ := hex.DecodeString(p.ETag)
		md5s = append(md5s, raw...)
		parts = append(parts, p)
		cparts = append(cparts, meta.CompletePart{Number: p.Number, ETag: p.ETag})
		sizes = append(sizes, strconv.FormatInt(p.Size, 10))
	}
	sum := md5.Sum(md5s) //nolint:gosec
	etag := hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(parts))

	var cks meta.Checksums
	if u.ChecksumAlgo != "" {
		val, err := multipartChecksum(u.ChecksumAlgo, u.ChecksumType, parts)
		if err != nil {
			return s3err.Newf(s3err.InvalidRequest, "%v", err)
		}
		store.SetChecksum(&cks, u.ChecksumAlgo, val)
		cks.Type = u.ChecksumType
		if want := c.r.Header.Get("x-amz-checksum-" + strings.ToLower(u.ChecksumAlgo)); want != "" && want != val {
			return s3err.Newf(s3err.BadDigest, "The %s you specified did not match the calculated checksum.", u.ChecksumAlgo)
		}
	}
	completeVID, completeNow := h.replicated(c, ids.VersionID())
	cmd := &meta.CompleteUpload{
		UploadID: u.ID, VersionID: completeVID, Parts: cparts, Now: completeNow, ETag: etag, Checksums: cks,
		IfNoneMatchAny: c.r.Header.Get("If-None-Match") == "*",
		ExtraMetadata:  map[string]string{metaPartSizes: strings.Join(sizes, ",")},
	}
	r, err := h.propose(c, cmd)
	if err != nil {
		return err
	}
	h.changed()
	var cr meta.CompleteUploadResult
	_ = r.Decode(&cr)
	h.emit(c, "s3:ObjectCreated:CompleteMultipartUpload", c.target.Key, cr.Size, etag, cr.VersionID)
	return h.completeResponse(c, etag, cr.VersionID, cks)
}

func (h *Handler) completeResponse(c *reqCtx, etag, vid string, cks meta.Checksums) error {
	versionedHeader(c, vid)
	scheme := "http"
	if c.r.TLS != nil {
		scheme = "https"
	}
	out := s3xml.CompleteMultipartUploadResult{
		Location: scheme + "://" + c.r.Host + "/" + c.bucket.Name + "/" + c.target.Key,
		Bucket:   c.bucket.Name, Key: c.target.Key, ETag: QuoteETag(etag),
	}
	out.Checksums = xmlChecksums(cks)
	out.ChecksumType = cks.Type
	return c.xml(http.StatusOK, &out)
}

func partChecksumFromRequest(p s3xml.CompletedPart, algo string) string {
	switch algo {
	case "CRC32":
		return p.ChecksumCRC32
	case "CRC32C":
		return p.ChecksumCRC32C
	case "SHA1":
		return p.ChecksumSHA1
	case "SHA256":
		return p.ChecksumSHA256
	case "CRC64NVME":
		return p.ChecksumCRC64NVME
	}
	return ""
}

func (h *Handler) abortMultipartUpload(c *reqCtx) error {
	u, err := h.loadUpload(c)
	if err != nil {
		return err
	}
	if _, err := h.propose(c, &meta.AbortUpload{UploadID: u.ID, Now: h.nowMs()}); err != nil {
		if errors.Is(err, meta.ErrNoSuchUpload) {
			return s3err.New(s3err.NoSuchUpload)
		}
		return err
	}
	return c.empty(http.StatusNoContent)
}

func (h *Handler) listParts(c *reqCtx) error {
	u, err := h.loadUpload(c)
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	max, err := ParseInt(q.Get("max-parts"), 1000, 0, 1000)
	if err != nil {
		return s3err.New(s3err.InvalidArgument).WithMessage("max-parts must be a non-negative integer")
	}
	marker, err := ParseInt(q.Get("part-number-marker"), 0, 0, 10000)
	if err != nil {
		return s3err.New(s3err.InvalidArgument).WithMessage("part-number-marker must be a non-negative integer")
	}
	parts, more, err := h.d.DB.ListParts(c.ctx(), u.ID, marker, max)
	if err != nil {
		return err
	}
	ow := h.owner(c, u.Owner)
	out := s3xml.ListPartsResult{
		Bucket: c.bucket.Name, Key: c.target.Key, UploadID: u.ID, Initiator: ow, Owner: ow,
		StorageClass: storageClassOf(u.StorageClass), PartNumberMarker: marker, MaxParts: max, IsTruncated: more,
		Part: []s3xml.PartEntry{}, ChecksumAlgorithm: u.ChecksumAlgo, ChecksumType: u.ChecksumType,
	}
	for _, p := range parts {
		e := s3xml.PartEntry{PartNumber: p.Number, LastModified: s3xml.FormatTime(p.Created), ETag: QuoteETag(p.ETag), Size: p.Size}
		e.Checksums = xmlChecksums(p.Checksums)
		out.Part = append(out.Part, e)
		out.NextPartNumberMarker = p.Number
	}
	return c.xml(http.StatusOK, &out)
}
