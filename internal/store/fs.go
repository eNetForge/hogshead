package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FS is the local filesystem chunk store.
type FS struct {
	root  string
	codec *Codec
}

// NewFS opens a filesystem store rooted at dir.
func NewFS(dir string, codec *Codec) (*FS, error) {
	if codec == nil {
		var err error
		if codec, err = NewCodec(false, nil); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o750); err != nil {
		return nil, err
	}
	return &FS{root: dir, codec: codec}, nil
}

// Root returns the store directory.
func (s *FS) Root() string { return s.root }

// Encrypted reports whether chunks are encrypted at rest.
func (s *FS) Encrypted() bool { return s.codec.Key != nil }

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *FS) path(hash string) string {
	return filepath.Join(s.root, hash[0:2], hash[2:4], hash)
}

// Put implements ChunkStore.
func (s *FS) Put(ctx context.Context, hash string, data []byte) error {
	if !validHash(hash) {
		return fmt.Errorf("invalid chunk hash %q", hash)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p := s.path(hash)
	if _, err := os.Stat(p); err == nil {
		now := time.Now()
		return os.Chtimes(p, now, now)
	}
	encoded, err := s.codec.Encode(hash, data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), hash[:8]+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if _, err := tmp.Write(encoded); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
		// A concurrent writer may have won; that is fine.
		if _, serr := os.Stat(p); serr == nil {
			return nil
		}
		return err
	}
	return syncDir(filepath.Dir(p))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// Get implements ChunkStore. The plaintext hash is verified on every read.
func (s *FS) Get(ctx context.Context, hash string) ([]byte, error) {
	if !validHash(hash) {
		return nil, ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stored, err := os.ReadFile(s.path(hash))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	plain, err := s.codec.Decode(hash, stored)
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", hash, err)
	}
	sum := sha256.Sum256(plain)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("chunk %s: %w: hash mismatch", hash, ErrCorrupt)
	}
	return plain, nil
}

// Has implements ChunkStore.
func (s *FS) Has(_ context.Context, hash string) (bool, error) {
	if !validHash(hash) {
		return false, nil
	}
	_, err := os.Stat(s.path(hash))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Stat implements ChunkStore.
func (s *FS) Stat(_ context.Context, hash string) (ChunkInfo, error) {
	if !validHash(hash) {
		return ChunkInfo{}, ErrNotFound
	}
	st, err := os.Stat(s.path(hash))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ChunkInfo{}, ErrNotFound
		}
		return ChunkInfo{}, err
	}
	return ChunkInfo{Hash: hash, Size: st.Size(), ModTime: st.ModTime()}, nil
}

// Touch implements ChunkStore.
func (s *FS) Touch(_ context.Context, hash string) error {
	if !validHash(hash) {
		return ErrNotFound
	}
	now := time.Now()
	err := os.Chtimes(s.path(hash), now, now)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// Delete implements ChunkStore.
func (s *FS) Delete(_ context.Context, hash string) error {
	if !validHash(hash) {
		return nil
	}
	err := os.Remove(s.path(hash))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Walk implements ChunkStore.
func (s *FS) Walk(ctx context.Context, fn func(ChunkInfo) error) error {
	return filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path != s.root && strings.HasPrefix(d.Name(), "tmp") {
				return filepath.SkipDir
			}
			return nil
		}
		if !validHash(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // deleted while walking
			}
			return err
		}
		return fn(ChunkInfo{Hash: d.Name(), Size: info.Size(), ModTime: info.ModTime()})
	})
}

// CleanTemp removes leftover temp files older than age.
func (s *FS) CleanTemp(age time.Duration) error {
	entries, err := os.ReadDir(filepath.Join(s.root, "tmp"))
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-age)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(s.root, "tmp", e.Name()))
		}
	}
	return nil
}
