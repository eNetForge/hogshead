package store

import (
	"context"
	"crypto/md5"  //nolint:gosec // S3 ETags are MD5 by protocol
	"crypto/sha1" //nolint:gosec // AWS checksum algorithm option
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/store/crc64nvme"
)

// WriteOpts controls which digests Write computes.
type WriteOpts struct {
	ChunkSize int
	// Algorithms lists extra AWS checksum algorithms to compute:
	// CRC32, CRC32C, SHA1, SHA256, CRC64NVME. MD5 is always computed.
	Algorithms []string
	// MaxSize aborts the write when exceeded (0 = unlimited).
	MaxSize int64
}

// WriteResult describes stored object data.
type WriteResult struct {
	Chunks    []meta.ChunkRef
	Size      int64
	MD5       string // hex
	Checksums meta.Checksums
}

// ErrTooLarge is returned when WriteOpts.MaxSize is exceeded.
var ErrTooLarge = errors.New("object too large")

// ErrTruncated is returned when the body ended before its declared end
// (for example a client disconnecting mid chunked transfer).
var ErrTruncated = errors.New("request body truncated")

// eofTracker records whether the underlying reader reached a clean io.EOF,
// which io.ReadFull would otherwise hide behind ErrUnexpectedEOF.
type eofTracker struct {
	r     io.Reader
	clean bool
}

func (e *eofTracker) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		e.clean = true
	}
	return n, err
}

// Write reads r to EOF, splits it into chunks, stores them and returns the
// chunk list and digests. Chunks of a failed write may remain on disk; the
// GC reclaims them because no metadata references them.
func Write(ctx context.Context, cs ChunkStore, r io.Reader, o WriteOpts) (*WriteResult, error) {
	if o.ChunkSize <= 0 {
		o.ChunkSize = DefaultChunkSize
	}
	md5h := md5.New() //nolint:gosec
	extras := map[string]hash.Hash{}
	for _, a := range o.Algorithms {
		h := newChecksum(a)
		if h == nil {
			return nil, fmt.Errorf("unsupported checksum algorithm %q", a)
		}
		extras[a] = h
	}
	res := &WriteResult{Chunks: []meta.ChunkRef{}}
	buf := make([]byte, o.ChunkSize)
	src := &eofTracker{r: r}
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			data := buf[:n]
			res.Size += int64(n)
			if o.MaxSize > 0 && res.Size > o.MaxSize {
				return nil, ErrTooLarge
			}
			md5h.Write(data)
			for _, h := range extras {
				h.Write(data)
			}
			sum := sha256.Sum256(data)
			hs := hex.EncodeToString(sum[:])
			if err := cs.Put(ctx, hs, data); err != nil {
				return nil, err
			}
			res.Chunks = append(res.Chunks, meta.ChunkRef{Hash: hs, Size: int64(n)})
		}
		if err != nil {
			if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && src.clean {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("%w: body ended unexpectedly", ErrTruncated)
			}
			return nil, err
		}
	}
	res.MD5 = hex.EncodeToString(md5h.Sum(nil))
	for a, h := range extras {
		setChecksum(&res.Checksums, a, base64.StdEncoding.EncodeToString(h.Sum(nil)))
	}
	if len(extras) > 0 {
		res.Checksums.Type = "FULL_OBJECT"
	}
	return res, nil
}

func newChecksum(algo string) hash.Hash {
	switch algo {
	case "CRC32":
		return crc32.NewIEEE()
	case "CRC32C":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "SHA1":
		return sha1.New() //nolint:gosec
	case "SHA256":
		return sha256.New()
	case "CRC64NVME":
		return crc64nvme.New()
	}
	return nil
}

// ChecksumValue returns the stored value for an algorithm.
func ChecksumValue(c meta.Checksums, algo string) string {
	switch algo {
	case "CRC32":
		return c.CRC32
	case "CRC32C":
		return c.CRC32C
	case "SHA1":
		return c.SHA1
	case "SHA256":
		return c.SHA256
	case "CRC64NVME":
		return c.CRC64NVME
	}
	return ""
}

func setChecksum(c *meta.Checksums, algo, v string) {
	switch algo {
	case "CRC32":
		c.CRC32 = v
	case "CRC32C":
		c.CRC32C = v
	case "SHA1":
		c.SHA1 = v
	case "SHA256":
		c.SHA256 = v
	case "CRC64NVME":
		c.CRC64NVME = v
	}
}

// SetChecksum sets a checksum value by algorithm name.
func SetChecksum(c *meta.Checksums, algo, v string) { setChecksum(c, algo, v) }

// Open returns a reader over the byte range [offset, offset+length) of an
// object made of chunks. length < 0 means to the end.
func Open(ctx context.Context, cs ChunkStore, chunks []meta.ChunkRef, offset, length int64) (io.ReadCloser, error) {
	var total int64
	for _, c := range chunks {
		total += c.Size
	}
	if offset < 0 || offset > total {
		return nil, fmt.Errorf("offset %d out of range [0,%d]", offset, total)
	}
	if length < 0 || offset+length > total {
		length = total - offset
	}
	return &objectReader{ctx: ctx, cs: cs, chunks: chunks, pos: offset, remaining: length}, nil
}

type objectReader struct {
	ctx       context.Context
	cs        ChunkStore
	chunks    []meta.ChunkRef
	pos       int64 // absolute position in object
	remaining int64
	idx       int   // current chunk index
	idxStart  int64 // absolute start of chunks[idx]
	cur       []byte
	curOff    int
	seeked    bool
}

func (r *objectReader) locate() error {
	// Advance idx until chunks[idx] contains pos.
	if !r.seeked {
		r.seeked = true
		r.idx, r.idxStart = 0, 0
		for r.idx < len(r.chunks) && r.idxStart+r.chunks[r.idx].Size <= r.pos {
			r.idxStart += r.chunks[r.idx].Size
			r.idx++
		}
	}
	if r.cur == nil {
		if r.idx >= len(r.chunks) {
			return io.EOF
		}
		data, err := r.cs.Get(r.ctx, r.chunks[r.idx].Hash)
		if err != nil {
			return err
		}
		if int64(len(data)) != r.chunks[r.idx].Size {
			return fmt.Errorf("chunk %s: %w: size %d, expected %d", r.chunks[r.idx].Hash, ErrCorrupt, len(data), r.chunks[r.idx].Size)
		}
		r.cur = data
		r.curOff = int(r.pos - r.idxStart)
	}
	return nil
}

func (r *objectReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if err := r.locate(); err != nil {
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n := copy(p, r.cur[r.curOff:])
	r.curOff += n
	r.pos += int64(n)
	r.remaining -= int64(n)
	if r.curOff >= len(r.cur) {
		r.idxStart += r.chunks[r.idx].Size
		r.idx++
		r.cur = nil
	}
	if n == 0 && r.remaining > 0 {
		return 0, fmt.Errorf("%w: short chunk", ErrCorrupt)
	}
	return n, nil
}

func (r *objectReader) Close() error { return nil }
