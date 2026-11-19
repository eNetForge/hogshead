package auth

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

const (
	// chunkReaderBufferSize bounds the length of a chunk header or trailer
	// line; longer lines are rejected as malformed.
	chunkReaderBufferSize = 8 << 10
	chunkSignaturePrefix  = "chunk-signature="
	trailerSignatureName  = "x-amz-trailer-signature"
	maxChunkSize          = int64(1) << 40
)

// NewChunkedReader wraps body for the streaming payload modes. It validates
// each chunk's signature (for signed modes) using id.SigningKey and
// id.SeedSignature, and collects (and for signed modes validates) the trailer
// headers. The returned reader yields the decoded payload; after io.EOF,
// Trailers returns the trailer header map. For PayloadSigned and
// PayloadUnsigned the body is returned unchanged with empty Trailers.
//
// Chunk signatures can only be checked once a chunk has been fully read, so
// data from a chunk is delivered before its signature is confirmed. Callers
// must discard the upload when Read returns any error other than io.EOF.
func NewChunkedReader(body io.Reader, id *Identity) ChunkedReader {
	if id == nil || !id.Payload.Streaming() {
		return plainReader{Reader: body}
	}
	return &chunkedReader{
		br:       bufio.NewReaderSize(body, chunkReaderBufferSize),
		key:      id.SigningKey,
		scope:    id.Scope,
		amzDate:  id.Date.UTC().Format(amzDateFormat),
		signed:   id.Payload != PayloadStreamingUnsignedTrailer,
		trailer:  id.Payload != PayloadStreaming,
		prevSig:  strings.ToLower(id.SeedSignature),
		hasher:   sha256.New(),
		trailers: http.Header{},
	}
}

// plainReader passes a non-streaming body through unchanged.
type plainReader struct{ io.Reader }

func (plainReader) Trailers() http.Header { return http.Header{} }

// chunkedReader decodes the aws-chunked framing:
//
//	<hex-size>[;chunk-signature=<sig>]\r\n<data>\r\n ... 0[;chunk-signature=<sig>]\r\n
//
// followed by either "\r\n" (no trailer) or trailer lines "name:value\r\n"
// terminated by an empty line.
type chunkedReader struct {
	br      *bufio.Reader
	key     []byte
	scope   string
	amzDate string
	signed  bool // chunks and trailer carry signatures
	trailer bool // trailer headers follow the final chunk

	prevSig   string
	hasher    hash.Hash
	chunkSize int64
	remaining int64
	expectSig string
	inChunk   bool
	done      bool
	err       error
	trailers  http.Header
}

func (c *chunkedReader) Trailers() http.Header { return c.trailers.Clone() }

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	for {
		switch {
		case c.done:
			return 0, io.EOF
		case !c.inChunk:
			if err := c.readChunkHeader(); err != nil {
				return c.fail(err)
			}
		case c.remaining == 0:
			if err := c.finishChunk(); err != nil {
				return c.fail(err)
			}
		default:
			n, err := c.readData(p)
			if err != nil {
				return c.fail(err)
			}
			if n > 0 {
				return n, nil
			}
		}
	}
}

func (c *chunkedReader) fail(err error) (int, error) {
	c.err = err
	return 0, err
}

// readData copies up to len(p) bytes of the current chunk into p.
func (c *chunkedReader) readData(p []byte) (int, error) {
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.br.Read(p)
	if n > 0 {
		if c.signed {
			c.hasher.Write(p[:n])
		}
		c.remaining -= int64(n)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return n, framingError("unexpected end of body inside chunk")
		}
		return n, err
	}
	return n, nil
}

// readChunkHeader parses "<hex-size>[;chunk-signature=<sig>]".
func (c *chunkedReader) readChunkHeader() error {
	line, err := c.readLine()
	if errors.Is(err, io.EOF) {
		return framingError("end of body before final chunk")
	}
	if err != nil {
		return err
	}
	sizeStr, ext, hasExt := strings.Cut(line, ";")
	size, err := strconv.ParseInt(sizeStr, 16, 64)
	if err != nil || size < 0 || size > maxChunkSize {
		return framingError("invalid chunk size %q", sizeStr)
	}
	if c.signed {
		sig, ok := strings.CutPrefix(ext, chunkSignaturePrefix)
		if !hasExt || !ok || !isHex(sig, 64) {
			return framingError("missing or invalid chunk-signature in %q", line)
		}
		c.expectSig = strings.ToLower(sig)
	}
	c.chunkSize = size
	c.remaining = size
	c.inChunk = true
	c.hasher.Reset()
	return nil
}

// finishChunk runs once all data of the current chunk has been consumed: it
// verifies the chunk signature and consumes the framing that follows.
func (c *chunkedReader) finishChunk() error {
	if c.signed {
		expected := c.chunkSignature(hex.EncodeToString(c.hasher.Sum(nil)))
		if !equalSignature(expected, c.expectSig) {
			return fmt.Errorf("%w: chunk signature", ErrSignatureMismatch)
		}
		c.prevSig = expected
	}
	c.inChunk = false
	if c.chunkSize > 0 || !c.trailer {
		if err := c.expectCRLF(); err != nil {
			return err
		}
	}
	if c.chunkSize == 0 {
		if c.trailer {
			if err := c.readTrailers(); err != nil {
				return err
			}
		}
		c.done = true
	}
	return nil
}

// readTrailers consumes "name:value" lines up to an empty line or EOF, and
// for signed modes verifies x-amz-trailer-signature.
func (c *chunkedReader) readTrailers() error {
	var canonical strings.Builder
	var trailerSig string
	for {
		line, err := c.readLine()
		if errors.Is(err, io.EOF) || (err == nil && line == "") {
			break
		}
		if err != nil {
			return err
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return framingError("invalid trailer line %q", line)
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == trailerSignatureName {
			trailerSig = value
			continue
		}
		c.trailers.Add(textproto.CanonicalMIMEHeaderKey(name), value)
		canonical.WriteString(name + ":" + value + "\n")
	}
	if !c.signed {
		return nil
	}
	if !isHex(trailerSig, 64) {
		return fmt.Errorf("%w: missing or invalid %s", ErrSignatureMismatch, trailerSignatureName)
	}
	expected := c.trailerSignature(sha256Hex([]byte(canonical.String())))
	if !equalSignature(expected, trailerSig) {
		return fmt.Errorf("%w: trailer signature", ErrSignatureMismatch)
	}
	c.prevSig = expected
	return nil
}

// chunkSignature computes the signature of a chunk whose data hashes to
// dataHash, chained from the previous signature.
func (c *chunkedReader) chunkSignature(dataHash string) string {
	return signHex(c.key, strings.Join([]string{
		algorithmPayload, c.amzDate, c.scope, c.prevSig, emptySHA256, dataHash,
	}, "\n"))
}

// trailerSignature computes the signature of the canonical trailer block.
func (c *chunkedReader) trailerSignature(trailerHash string) string {
	return signHex(c.key, strings.Join([]string{
		algorithmTrailer, c.amzDate, c.scope, c.prevSig, trailerHash,
	}, "\n"))
}

// readLine reads one line terminated by "\n" (a preceding "\r" is stripped).
// io.EOF is returned only when no bytes were read; a partial line is a
// framing error.
func (c *chunkedReader) readLine() (string, error) {
	raw, err := c.br.ReadSlice('\n')
	if err != nil {
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			return "", framingError("line exceeds %d bytes", chunkReaderBufferSize)
		case errors.Is(err, io.EOF) && len(raw) == 0:
			return "", io.EOF
		case errors.Is(err, io.EOF):
			return "", framingError("unexpected end of body in chunk header")
		}
		return "", err
	}
	return strings.TrimSuffix(string(raw[:len(raw)-1]), "\r"), nil
}

// expectCRLF consumes the "\r\n" that terminates chunk data.
func (c *chunkedReader) expectCRLF() error {
	var buf [2]byte
	if _, err := io.ReadFull(c.br, buf[:]); err != nil {
		return framingError("missing CRLF after chunk data")
	}
	if buf != [2]byte{'\r', '\n'} {
		return framingError("expected CRLF after chunk data, got %q", buf[:])
	}
	return nil
}

func framingError(format string, args ...any) error {
	return fmt.Errorf("%w: aws-chunked framing: %s", ErrMalformed, fmt.Sprintf(format, args...))
}
