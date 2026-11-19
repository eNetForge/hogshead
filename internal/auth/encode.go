package auth

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// EncodeChunked frames data as an aws-chunked body matching id.Payload
// (signed, signed with trailer, or unsigned with trailer), using chunks of
// chunkSize bytes. trailers are appended for the trailer modes. It is used by
// tests and by the replication client.
func EncodeChunked(data []byte, id *Identity, trailers map[string]string, chunkSize int) []byte {
	if chunkSize <= 0 {
		chunkSize = 65536
	}
	e := &chunkEncoder{id: id, prev: id.SeedSignature}
	var buf bytes.Buffer
	for off := 0; off < len(data); off += chunkSize {
		end := off + chunkSize
		if end > len(data) {
			end = len(data)
		}
		e.chunk(&buf, data[off:end])
	}
	if e.signed() {
		fmt.Fprintf(&buf, "0;chunk-signature=%s\r\n", e.chunkSig(nil))
	} else {
		buf.WriteString("0\r\n")
	}
	if id.Payload == PayloadStreaming {
		buf.WriteString("\r\n")
		return buf.Bytes()
	}
	names := make([]string, 0, len(trailers))
	for k := range trailers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, k := range names {
		fmt.Fprintf(&buf, "%s:%s\r\n", k, trailers[k])
		canonical.WriteString(strings.ToLower(k) + ":" + trailers[k] + "\n")
	}
	if e.signed() {
		fmt.Fprintf(&buf, "%s:%s\r\n", trailerSignatureName, e.trailerSig(canonical.String()))
	}
	buf.WriteString("\r\n")
	return buf.Bytes()
}

// EncodeChunkedForTest is an alias kept for test readability.
func EncodeChunkedForTest(data []byte, id *Identity, trailers map[string]string, chunkSize int) []byte {
	return EncodeChunked(data, id, trailers, chunkSize)
}

// chunkEncoder produces aws-chunked framing with the same primitives the
// verifier uses.
type chunkEncoder struct {
	id   *Identity
	prev string
}

func (e *chunkEncoder) signed() bool { return e.id.Payload != PayloadStreamingUnsignedTrailer }

func (e *chunkEncoder) chunkSig(data []byte) string {
	sts := strings.Join([]string{algorithmPayload, e.id.Date.UTC().Format(amzDateFormat), e.id.Scope, e.prev, emptySHA256, sha256Hex(data)}, "\n")
	e.prev = signHex(e.id.SigningKey, sts)
	return e.prev
}

func (e *chunkEncoder) trailerSig(canonical string) string {
	sts := strings.Join([]string{algorithmTrailer, e.id.Date.UTC().Format(amzDateFormat), e.id.Scope, e.prev, sha256Hex([]byte(canonical))}, "\n")
	e.prev = signHex(e.id.SigningKey, sts)
	return e.prev
}

func (e *chunkEncoder) chunk(buf *bytes.Buffer, data []byte) {
	if e.signed() {
		fmt.Fprintf(buf, "%x;chunk-signature=%s\r\n", len(data), e.chunkSig(data))
	} else {
		fmt.Fprintf(buf, "%x\r\n", len(data))
	}
	buf.Write(data)
	buf.WriteString("\r\n")
}
