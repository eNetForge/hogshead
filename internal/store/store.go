// Package store is the content-addressed chunk store. Chunks are immutable
// blobs named by the hex SHA-256 of their plaintext. Object data is a list of
// chunk references held in metadata.
package store

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned when a chunk does not exist.
var ErrNotFound = errors.New("chunk not found")

// ErrCorrupt is returned when a stored chunk fails verification.
var ErrCorrupt = errors.New("chunk corrupt")

// DefaultChunkSize is the default chunk size.
const DefaultChunkSize = 4 << 20

// ChunkInfo describes a stored chunk for inventory walks.
type ChunkInfo struct {
	Hash    string
	Size    int64 // size on disk (encoded)
	ModTime time.Time
}

// ChunkStore is the storage interface implemented by the local filesystem
// backend.
type ChunkStore interface {
	// Put stores data under hash. It is idempotent: storing an existing
	// chunk is a no-op that refreshes its modification time.
	Put(ctx context.Context, hash string, data []byte) error
	// Get returns the plaintext of a chunk.
	Get(ctx context.Context, hash string) ([]byte, error)
	// Has reports whether a chunk exists locally.
	Has(ctx context.Context, hash string) (bool, error)
	// Delete removes a chunk. Deleting a missing chunk is not an error.
	Delete(ctx context.Context, hash string) error
	// Walk calls fn for every stored chunk. Returning an error stops the walk.
	Walk(ctx context.Context, fn func(ChunkInfo) error) error
	// Touch refreshes the modification time so GC grace periods restart.
	Touch(ctx context.Context, hash string) error
	// Stat returns size and modification time of a local chunk.
	Stat(ctx context.Context, hash string) (ChunkInfo, error)
}

// ReadSeekCloser is what Open returns.
type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}
