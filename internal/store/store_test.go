package store

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestCodecRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	compressible := bytes.Repeat([]byte("hello world "), 1000)
	random := make([]byte, 5000)
	_, _ = rand.Read(random)
	for _, tc := range []struct {
		name     string
		compress bool
		key      []byte
	}{
		{"plain", false, nil}, {"zstd", true, nil}, {"aes", false, key}, {"zstd+aes", true, key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCodec(tc.compress, tc.key)
			if err != nil {
				t.Fatal(err)
			}
			for _, data := range [][]byte{compressible, random, {}, {1}} {
				h := hashOf(data)
				enc, err := c.Encode(h, data)
				if err != nil {
					t.Fatal(err)
				}
				if tc.compress && len(data) > 1000 && bytes.Equal(data, compressible) && len(enc) >= len(data) {
					t.Fatal("compressible data was not compressed")
				}
				if !tc.compress && len(enc) != len(data)+1+map[bool]int{true: 16, false: 0}[tc.key != nil] {
					t.Fatalf("unexpected encoded size %d for %d", len(enc), len(data))
				}
				dec, err := c.Decode(h, enc)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(dec, data) {
					t.Fatal("round trip mismatch")
				}
			}
			// Deterministic.
			a, _ := c.Encode(hashOf(random), random)
			b, _ := c.Encode(hashOf(random), random)
			if !bytes.Equal(a, b) {
				t.Fatal("encoding must be deterministic")
			}
		})
	}
	if _, err := NewCodec(false, []byte("short")); err == nil {
		t.Fatal("short key must fail")
	}
	c, _ := NewCodec(false, key)
	h := hashOf(random)
	enc, _ := c.Encode(h, random)
	enc[len(enc)-1] ^= 1
	if _, err := c.Decode(h, enc); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered ciphertext: %v", err)
	}
	plain, _ := NewCodec(false, nil)
	if _, err := plain.Decode(h, enc); err == nil {
		t.Fatal("decoding encrypted chunk without key must fail")
	}
	if _, err := plain.Decode(h, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatal("empty")
	}
	if _, err := plain.Decode(h, []byte{0xf0, 1}); !errors.Is(err, ErrCorrupt) {
		t.Fatal("unknown flags")
	}
}

func TestFSStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewFS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("some chunk data")
	h := hashOf(data)
	if ok, _ := s.Has(ctx, h); ok {
		t.Fatal("should not exist yet")
	}
	if _, err := s.Get(ctx, h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := s.Put(ctx, h, data); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, h, data); err != nil {
		t.Fatal("idempotent put failed:", err)
	}
	if ok, _ := s.Has(ctx, h); !ok {
		t.Fatal("should exist")
	}
	got, err := s.Get(ctx, h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get: %v %q", err, got)
	}
	if err := s.Put(ctx, "nothex", data); err == nil {
		t.Fatal("invalid hash must be rejected")
	}
	if _, err := s.Get(ctx, "nothex"); !errors.Is(err, ErrNotFound) {
		t.Fatal("invalid hash get")
	}
	// Corrupt on disk: hash mismatch detected.
	p := s.path(h)
	_ = os.WriteFile(p, []byte{0, 'x'}, 0o600)
	if _, err := s.Get(ctx, h); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corruption not detected: %v", err)
	}
	_ = s.Delete(ctx, h)
	_ = s.Put(ctx, h, data)
	// Walk sees it; temp dir ignored.
	_ = os.WriteFile(filepath.Join(dir, "tmp", "leftover"), []byte("x"), 0o600)
	var seen []ChunkInfo
	if err := s.Walk(ctx, func(ci ChunkInfo) error { seen = append(seen, ci); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Hash != h || seen[0].Size != int64(len(data))+1 {
		t.Fatalf("walk: %+v", seen)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "tmp", "leftover"), old, old)
	if err := s.CleanTemp(time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp", "leftover")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp not cleaned")
	}
	_ = os.Chtimes(p, old, old)
	if err := s.Touch(ctx, h); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Fatal("touch did not update mtime")
	}
	if err := s.Touch(ctx, hashOf([]byte("missing"))); !errors.Is(err, ErrNotFound) {
		t.Fatal("touch missing")
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatal("delete missing must be nil")
	}
	if err := s.Delete(ctx, "bad"); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Put(cctx, h, data); err == nil {
		t.Fatal("cancelled context")
	}
	if _, err := s.Get(cctx, h); err == nil {
		t.Fatal("cancelled context")
	}
}

func TestWriteAndOpen(t *testing.T) {
	ctx := context.Background()
	s, _ := NewFS(t.TempDir(), nil)
	data := make([]byte, 10_000)
	_, _ = rand.Read(data)
	res, err := Write(ctx, s, bytes.NewReader(data), WriteOpts{ChunkSize: 4096, Algorithms: []string{"CRC32C", "SHA256", "CRC32", "SHA1", "CRC64NVME"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != 10_000 || len(res.Chunks) != 3 || res.Chunks[2].Size != 10_000-8192 {
		t.Fatalf("chunks: %+v", res)
	}
	m := md5.Sum(data) //nolint:gosec
	if res.MD5 != hex.EncodeToString(m[:]) {
		t.Fatal("md5")
	}
	if res.Checksums.CRC32C == "" || res.Checksums.SHA256 == "" || res.Checksums.CRC32 == "" || res.Checksums.SHA1 == "" || res.Checksums.CRC64NVME == "" || res.Checksums.Type != "FULL_OBJECT" {
		t.Fatalf("checksums: %+v", res.Checksums)
	}
	if ChecksumValue(res.Checksums, "CRC32C") != res.Checksums.CRC32C || ChecksumValue(res.Checksums, "nope") != "" {
		t.Fatal("ChecksumValue")
	}
	// Dedup: same content, same chunks.
	res2, _ := Write(ctx, s, bytes.NewReader(data), WriteOpts{ChunkSize: 4096})
	if res2.Chunks[0].Hash != res.Chunks[0].Hash {
		t.Fatal("content addressing broken")
	}
	// Full read.
	rc, err := Open(ctx, s, res.Chunks, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatal("full read mismatch")
	}
	// Ranges crossing chunk boundaries and at the edges.
	for _, r := range [][2]int64{{0, 1}, {4095, 2}, {4096, 4096}, {8000, 2000}, {9999, 1}, {0, 10000}, {5000, -1}, {10000, 0}} {
		rc, err := Open(ctx, s, res.Chunks, r[0], r[1])
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		end := r[0] + r[1]
		if r[1] < 0 {
			end = 10000
		}
		if !bytes.Equal(got, data[r[0]:end]) {
			t.Fatalf("range %v mismatch (%d bytes)", r, len(got))
		}
	}
	if _, err := Open(ctx, s, res.Chunks, 10001, 1); err == nil {
		t.Fatal("offset past end")
	}
	// Empty object.
	e, err := Write(ctx, s, bytes.NewReader(nil), WriteOpts{})
	if err != nil || e.Size != 0 || len(e.Chunks) != 0 || e.MD5 != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("empty: %+v %v", e, err)
	}
	rc, _ = Open(ctx, s, e.Chunks, 0, -1)
	if b, _ := io.ReadAll(rc); len(b) != 0 {
		t.Fatal("empty read")
	}
	// Size limit.
	if _, err := Write(ctx, s, bytes.NewReader(data), WriteOpts{MaxSize: 100}); !errors.Is(err, ErrTooLarge) {
		t.Fatal("max size")
	}
	if _, err := Write(ctx, s, bytes.NewReader(data), WriteOpts{Algorithms: []string{"MD6"}}); err == nil {
		t.Fatal("bad algorithm")
	}
	// Missing chunk surfaces on read.
	_ = s.Delete(ctx, res.Chunks[1].Hash)
	rc, _ = Open(ctx, s, res.Chunks, 0, -1)
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing chunk: %v", err)
	}
	// Size mismatch between metadata and chunk.
	bad := []meta.ChunkRef{{Hash: res.Chunks[0].Hash, Size: 1}}
	rc, _ = Open(ctx, s, bad, 0, -1)
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("size mismatch: %v", err)
	}
	var c meta.Checksums
	SetChecksum(&c, "SHA1", "x")
	if c.SHA1 != "x" {
		t.Fatal("SetChecksum")
	}
}

func TestWriteErrors(t *testing.T) {
	s, _ := NewFS(t.TempDir(), nil)
	r := io.MultiReader(bytes.NewReader([]byte("abc")), errReader{})
	if _, err := Write(context.Background(), s, r, WriteOpts{}); err == nil {
		t.Fatal("reader error must propagate")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
