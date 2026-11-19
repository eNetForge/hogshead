package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// On-disk chunk format: one flags byte followed by the encoded payload.
const (
	flagZstd   byte = 1 << 0
	flagAESGCM byte = 1 << 1
)

// Codec transforms plaintext chunks to their stored form and back.
type Codec struct {
	Compress bool
	// Key is the 32-byte master key for encryption at rest; nil disables.
	Key []byte

	enc *zstd.Encoder
	dec *zstd.Decoder
}

// NewCodec builds a codec. Key must be nil or 32 bytes.
func NewCodec(compress bool, key []byte) (*Codec, error) {
	if key != nil && len(key) != 32 {
		return nil, errors.New("encryption key must be 32 bytes")
	}
	c := &Codec{Compress: compress, Key: key}
	var err error
	if c.enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1)); err != nil {
		return nil, err
	}
	if c.dec, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1)); err != nil {
		return nil, err
	}
	return c, nil
}

// chunkKey derives a per-chunk AES key from the master key, the chunk hash
// and the encoding flags. The key is unique per (plaintext, encoding), so a
// fixed nonce is safe and encryption stays deterministic, which keeps
// replication idempotent. Flags are mixed in because the compressed and the
// raw forms of the same chunk are different plaintexts for GCM.
func (c *Codec) chunkKey(hash string, flags byte) []byte {
	m := hmac.New(sha256.New, c.Key)
	m.Write([]byte("minion-s3 chunk key v2\x00")) // historical label; changing it would re-key every encrypted chunk
	m.Write([]byte(hash))
	m.Write([]byte{flags})
	return m.Sum(nil)
}

func aad(hash string, flags byte) []byte {
	return append([]byte(hash), flags)
}

var fixedNonce = make([]byte, 12)

// Encode produces the on-disk representation of plain.
func (c *Codec) Encode(hash string, plain []byte) ([]byte, error) {
	var flags byte
	payload := plain
	if c.Compress {
		compressed := c.enc.EncodeAll(plain, make([]byte, 0, len(plain)/2))
		// Only keep compression when it actually helps.
		if len(compressed) < len(plain)-len(plain)/16 {
			payload = compressed
			flags |= flagZstd
		}
	}
	if c.Key != nil {
		flags |= flagAESGCM
		block, err := aes.NewCipher(c.chunkKey(hash, flags))
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		payload = gcm.Seal(nil, fixedNonce, payload, aad(hash, flags)) //nolint:gosec // the key is unique per chunk and encoding, so a fixed nonce is safe (see chunkKey)
	}
	out := make([]byte, 0, 1+len(payload))
	out = append(out, flags)
	return append(out, payload...), nil
}

// Decode reverses Encode. It needs the master key if the chunk was encrypted.
func (c *Codec) Decode(hash string, stored []byte) ([]byte, error) {
	if len(stored) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrCorrupt)
	}
	flags, payload := stored[0], stored[1:]
	if flags&^(flagZstd|flagAESGCM) != 0 {
		return nil, fmt.Errorf("%w: unknown flags %#x", ErrCorrupt, flags)
	}
	if flags&flagAESGCM != 0 {
		if c.Key == nil {
			return nil, errors.New("chunk is encrypted but no key is configured")
		}
		block, err := aes.NewCipher(c.chunkKey(hash, flags))
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		payload, err = gcm.Open(nil, fixedNonce, payload, aad(hash, flags))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
	}
	if flags&flagZstd != 0 {
		var err error
		payload, err = c.dec.DecodeAll(payload, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
	}
	return payload, nil
}
