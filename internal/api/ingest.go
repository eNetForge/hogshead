package api

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/enetforge/hogshead/internal/api/s3err"
	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/store"
)

// checksumAlgos lists the AWS additional checksum algorithms.
var checksumAlgos = []string{"CRC32", "CRC32C", "SHA1", "SHA256", "CRC64NVME"}

func normalizeAlgo(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	for _, a := range checksumAlgos {
		if a == s {
			return a
		}
	}
	return ""
}

// requestedChecksums returns algorithms the client sent values for in
// headers (algo -> expected base64), plus algorithms announced for trailers.
func requestedChecksums(h http.Header) (headers map[string]string, trailers []string, err error) {
	headers = map[string]string{}
	for _, a := range checksumAlgos {
		if v := h.Get("x-amz-checksum-" + strings.ToLower(a)); v != "" {
			headers[a] = v
		}
	}
	if a := h.Get("x-amz-sdk-checksum-algorithm"); a != "" {
		na := normalizeAlgo(a)
		if na == "" {
			return nil, nil, s3err.New(s3err.InvalidRequest).WithMessage("Unsupported checksum algorithm")
		}
		if _, ok := headers[na]; !ok {
			trailers = append(trailers, na)
		}
	}
	for _, t := range strings.Split(h.Get("x-amz-trailer"), ",") {
		t = strings.TrimSpace(strings.ToLower(t))
		if a := normalizeAlgo(strings.TrimPrefix(t, "x-amz-checksum-")); a != "" && strings.HasPrefix(t, "x-amz-checksum-") {
			found := false
			for _, x := range trailers {
				if x == a {
					found = true
				}
			}
			if !found {
				trailers = append(trailers, a)
			}
		}
	}
	if len(headers) > 1 {
		return nil, nil, s3err.New(s3err.InvalidRequest).WithMessage("Expecting a single x-amz-checksum- header")
	}
	return headers, trailers, nil
}

// ingest reads the request body into the chunk store and verifies every
// integrity claim the client made. extraAlgos are algorithms the caller
// wants computed regardless of what the client asked for.
func (h *Handler) ingest(c *reqCtx, extraAlgos []string, maxSize int64) (*store.WriteResult, error) {
	hdr := c.r.Header
	expected, trailerAlgos, err := requestedChecksums(hdr)
	if err != nil {
		return nil, err
	}
	algoSet := map[string]bool{}
	for a := range expected {
		algoSet[a] = true
	}
	for _, a := range trailerAlgos {
		algoSet[a] = true
	}
	for _, a := range extraAlgos {
		if a != "" {
			algoSet[a] = true
		}
	}
	verifySHA := false
	if c.ident != nil && c.ident.Payload == auth.PayloadSigned && len(c.ident.ContentSHA256) == 64 {
		verifySHA = true
		algoSet["SHA256"] = true
	}
	var algos []string
	for a := range algoSet {
		algos = append(algos, a)
	}

	var body io.Reader = c.r.Body
	var trailers func() http.Header
	expectLen := c.r.ContentLength
	if c.ident != nil && c.ident.Payload.Streaming() {
		cr := auth.NewChunkedReader(c.r.Body, c.ident)
		body = cr
		trailers = cr.Trailers
		if dl := hdr.Get("x-amz-decoded-content-length"); dl != "" {
			n, perr := strconv.ParseInt(dl, 10, 64)
			if perr != nil || n < 0 {
				return nil, s3err.New(s3err.InvalidArgument).WithMessage("Invalid x-amz-decoded-content-length")
			}
			expectLen = n
		} else {
			expectLen = -1
		}
	} else if expectLen < 0 && len(c.r.TransferEncoding) == 0 && c.r.Method == http.MethodPut {
		return nil, s3err.New(s3err.MissingContentLength)
	}
	if expectLen > maxSize && maxSize > 0 {
		return nil, s3err.New(s3err.EntityTooLarge)
	}

	res, err := store.Write(c.ctx(), h.d.Chunks, body, store.WriteOpts{
		ChunkSize: h.d.Cfg().Storage.ChunkSize, Algorithms: algos, MaxSize: maxSize,
	})
	if err != nil {
		if isAuthErr(err) {
			return nil, mapAuthError(err)
		}
		return nil, err
	}
	if h.d.BytesIn != nil {
		h.d.BytesIn(float64(res.Size))
	}
	if expectLen >= 0 && res.Size != expectLen {
		return nil, s3err.New(s3err.IncompleteBody)
	}
	if cm := hdr.Get("Content-MD5"); cm != "" {
		want, derr := base64.StdEncoding.DecodeString(cm)
		got, _ := hex.DecodeString(res.MD5)
		if derr != nil || len(want) != 16 {
			return nil, s3err.New(s3err.InvalidDigest)
		}
		if subtle.ConstantTimeCompare(want, got) != 1 {
			return nil, s3err.New(s3err.BadDigest)
		}
	}
	if verifySHA {
		want, _ := base64.StdEncoding.DecodeString(res.Checksums.SHA256)
		got, derr := hex.DecodeString(c.ident.ContentSHA256)
		if derr != nil || subtle.ConstantTimeCompare(want, got) != 1 {
			return nil, s3err.New(s3err.XAmzContentSHA256Mismatch)
		}
	}
	if trailers != nil {
		th := trailers()
		for _, a := range trailerAlgos {
			if v := th.Get("x-amz-checksum-" + strings.ToLower(a)); v != "" {
				expected[a] = v
			} else {
				return nil, s3err.New(s3err.InvalidRequest).WithMessage("Missing checksum trailer")
			}
		}
	}
	for a, want := range expected {
		if store.ChecksumValue(res.Checksums, a) != want {
			return nil, s3err.Newf(s3err.BadDigest, "The %s you specified did not match the calculated checksum.", a)
		}
	}
	// Keep only checksums the client asked for (or the caller requires).
	keep := map[string]bool{}
	for a := range expected {
		keep[a] = true
	}
	for _, a := range extraAlgos {
		keep[a] = true
	}
	var kept meta.Checksums
	for a := range keep {
		store.SetChecksum(&kept, a, store.ChecksumValue(res.Checksums, a))
	}
	if len(keep) > 0 {
		kept.Type = "FULL_OBJECT"
	}
	res.Checksums = kept
	return res, nil
}

func isAuthErr(err error) bool {
	for _, e := range []error{auth.ErrSignatureMismatch, auth.ErrMalformed, auth.ErrExpired} {
		if errorsIs(err, e) {
			return true
		}
	}
	return false
}

// setChecksumHeaders writes x-amz-checksum-* response headers.
func setChecksumHeaders(h http.Header, cks meta.Checksums) {
	for _, a := range checksumAlgos {
		if v := store.ChecksumValue(cks, a); v != "" {
			h.Set("x-amz-checksum-"+strings.ToLower(a), v)
		}
	}
	if cks.Type != "" && checksumAlgoOf(cks) != "" {
		h.Set("x-amz-checksum-type", cks.Type)
	}
}
