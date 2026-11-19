package api_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"

	"github.com/enetforge/hogshead/internal/store/crc64nvme"
)

var _ = context.Background

func testutilCRC64(b []byte) string {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, crc64nvme.Checksum(b))
	return base64.StdEncoding.EncodeToString(out)
}

type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

func ioNopCloser(r io.Reader) io.ReadCloser { return nopCloser{r} }
