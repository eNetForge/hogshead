package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// chunkEncoder is a test-side aws-chunked encoder built on the same signing
// primitives as the verifier, producing the three streaming formats.
func newEncoder(id *Identity) *chunkEncoder {
	return &chunkEncoder{id: id, prev: id.SeedSignature}
}

// encode frames chunks and trailers. Trailers are name/value pairs.
func (e *chunkEncoder) encode(chunks [][]byte, trailers [][2]string) []byte {
	var buf bytes.Buffer
	for _, c := range chunks {
		e.chunk(&buf, c)
	}
	if e.signed() {
		fmt.Fprintf(&buf, "0;chunk-signature=%s\r\n", e.chunkSig(nil))
	} else {
		buf.WriteString("0\r\n")
	}
	if e.id.Payload == PayloadStreaming {
		buf.WriteString("\r\n")
		return buf.Bytes()
	}
	var canonical strings.Builder
	for _, tr := range trailers {
		fmt.Fprintf(&buf, "%s:%s\r\n", tr[0], tr[1])
		canonical.WriteString(strings.ToLower(tr[0]) + ":" + tr[1] + "\n")
	}
	if e.signed() {
		fmt.Fprintf(&buf, "%s:%s\r\n", trailerSignatureName, e.trailerSig(canonical.String()))
	}
	buf.WriteString("\r\n")
	return buf.Bytes()
}

// streamingIdentity signs a PUT with the given streaming content-sha256 and
// verifies it, returning the Identity the API layer would hand to the reader.
func streamingIdentity(t *testing.T, mode string) *Identity {
	t.Helper()
	r := httptest.NewRequest("PUT", "http://localhost:9000/bucket/object", nil)
	r.Header.Set(headerContentSHA256, mode)
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", "100")
	if err := Sign(r, testCreds, suiteRegion, "s3", suiteTime, ""); err != nil {
		t.Fatal(err)
	}
	return mustVerify(t, suiteVerifier(suiteTime), r)
}

var sampleChunks = [][]byte{
	bytes.Repeat([]byte("a"), 1000),
	[]byte("hello, "),
	bytes.Repeat([]byte("z"), 20000),
	[]byte("world"),
}

func joined(chunks [][]byte) []byte { return bytes.Join(chunks, nil) }

func TestChunkedReaderModes(t *testing.T) {
	trailers := [][2]string{{"x-amz-checksum-crc32c", "sOO8/Q=="}}
	cases := []struct {
		mode     string
		want     PayloadMode
		trailers [][2]string
	}{
		{streamingSigned, PayloadStreaming, nil},
		{streamingSignedTrailer, PayloadStreamingTrailer, trailers},
		{streamingUnsignedTrailer, PayloadStreamingUnsignedTrailer, trailers},
	}
	for _, c := range cases {
		for _, readSize := range []int{1, 7, 512, 1 << 16} {
			t.Run(fmt.Sprintf("%s/read%d", c.mode, readSize), func(t *testing.T) {
				id := streamingIdentity(t, c.mode)
				if id.Payload != c.want {
					t.Fatalf("payload mode = %v, want %v", id.Payload, c.want)
				}
				body := newEncoder(id).encode(sampleChunks, c.trailers)
				cr := NewChunkedReader(bytes.NewReader(body), id)
				var out bytes.Buffer
				buf := make([]byte, readSize)
				for {
					n, err := cr.Read(buf)
					out.Write(buf[:n])
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Fatalf("Read: %v", err)
					}
				}
				if !bytes.Equal(out.Bytes(), joined(sampleChunks)) {
					t.Errorf("decoded %d bytes, want %d", out.Len(), len(joined(sampleChunks)))
				}
				got := cr.Trailers()
				if len(c.trailers) == 0 && len(got) != 0 {
					t.Errorf("unexpected trailers %v", got)
				}
				for _, tr := range c.trailers {
					if got.Get(tr[0]) != tr[1] {
						t.Errorf("trailer %s = %q, want %q", tr[0], got.Get(tr[0]), tr[1])
					}
				}
				// Subsequent reads keep returning EOF.
				if n, err := cr.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
					t.Errorf("read after EOF = %d, %v", n, err)
				}
			})
		}
	}
}

func TestChunkedReaderEmptyBodyAndOneByteReader(t *testing.T) {
	id := streamingIdentity(t, streamingSignedTrailer)
	body := newEncoder(id).encode(nil, [][2]string{{"x-amz-checksum-sha256", "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="}})
	cr := NewChunkedReader(iotest.OneByteReader(bytes.NewReader(body)), id)
	data, err := io.ReadAll(cr)
	if err != nil || len(data) != 0 {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
	if cr.Trailers().Get("X-Amz-Checksum-Sha256") == "" {
		t.Error("trailer missing")
	}
}

func TestChunkedReaderTrailerVariantsValid(t *testing.T) {
	id := streamingIdentity(t, streamingUnsignedTrailer)
	body := "5\r\nhello\r\n0\nx-amz-checksum-crc32:abcd\nX-Amz-Checksum-Sha1: Zm9v \n"
	cr := NewChunkedReader(strings.NewReader(body), id)
	data, err := io.ReadAll(cr)
	if err != nil || string(data) != "hello" {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
	tr := cr.Trailers()
	if tr.Get("X-Amz-Checksum-Crc32") != "abcd" || tr.Get("X-Amz-Checksum-Sha1") != "Zm9v" {
		t.Errorf("trailers = %v", tr)
	}
}

func TestChunkedReaderPassthrough(t *testing.T) {
	for _, id := range []*Identity{nil, {Payload: PayloadSigned}, {Payload: PayloadUnsigned}} {
		cr := NewChunkedReader(strings.NewReader("raw body"), id)
		data, err := io.ReadAll(cr)
		if err != nil || string(data) != "raw body" {
			t.Fatalf("ReadAll = %q, %v", data, err)
		}
		if len(cr.Trailers()) != 0 {
			t.Error("passthrough must have no trailers")
		}
	}
}

func TestChunkedReaderZeroLengthRead(t *testing.T) {
	id := streamingIdentity(t, streamingSigned)
	cr := NewChunkedReader(bytes.NewReader(newEncoder(id).encode(sampleChunks[:1], nil)), id)
	if n, err := cr.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v", n, err)
	}
}

func TestChunkedReaderSignatureErrors(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		mutate func(id *Identity, body []byte) []byte
	}{
		{"tampered chunk data", streamingSigned, func(_ *Identity, b []byte) []byte {
			out := bytes.Clone(b)
			out[bytes.Index(out, []byte("aaaa"))] = 'b'
			return out
		}},
		{"wrong seed", streamingSigned, func(id *Identity, b []byte) []byte {
			id.SeedSignature = strings.Repeat("0", 64)
			return b
		}},
		{"wrong key", streamingSignedTrailer, func(id *Identity, b []byte) []byte {
			id.SigningKey = []byte("nope")
			return b
		}},
		{"tampered trailer value", streamingSignedTrailer, func(_ *Identity, b []byte) []byte {
			return bytes.Replace(b, []byte("sOO8/Q=="), []byte("sOO8/R=="), 1)
		}},
		{"missing trailer signature", streamingSignedTrailer, func(_ *Identity, b []byte) []byte {
			i := bytes.Index(b, []byte(trailerSignatureName))
			return append(bytes.Clone(b[:i]), "\r\n"...)
		}},
		{"replayed chunk", streamingSigned, func(_ *Identity, b []byte) []byte {
			// Duplicate the first framed chunk: its signature chains from the
			// seed, so the second copy fails.
			end := bytes.Index(b, []byte("\r\n"))
			end = bytes.Index(b[end+2:], []byte("\r\n")) + end + 4
			return append(bytes.Clone(b[:end]), b...)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := streamingIdentity(t, c.mode)
			body := newEncoder(id).encode(sampleChunks, [][2]string{{"x-amz-checksum-crc32c", "sOO8/Q=="}})
			body = c.mutate(id, body)
			cr := NewChunkedReader(bytes.NewReader(body), id)
			_, err := io.ReadAll(cr)
			wantErr(t, err, ErrSignatureMismatch)
			// Error is sticky.
			if _, err2 := cr.Read(make([]byte, 1)); !errors.Is(err2, ErrSignatureMismatch) {
				t.Errorf("second read = %v", err2)
			}
		})
	}
}

func TestChunkedReaderFramingErrors(t *testing.T) {
	sigLine := func(id *Identity) string {
		return fmt.Sprintf("5;chunk-signature=%s\r\n", newEncoder(id).chunkSig([]byte("hello")))
	}
	cases := []struct {
		name string
		mode string
		body func(id *Identity) string
	}{
		{"bad hex size", streamingSigned, func(id *Identity) string { return "zz;chunk-signature=" + strings.Repeat("0", 64) + "\r\n" }},
		{"negative size", streamingSigned, func(id *Identity) string { return "-5;chunk-signature=" + strings.Repeat("0", 64) + "\r\n" }},
		{"huge size", streamingUnsignedTrailer, func(id *Identity) string { return "ffffffffffffff\r\n" }},
		{"missing chunk signature", streamingSigned, func(id *Identity) string { return "5\r\nhello\r\n" }},
		{"bad chunk signature ext", streamingSigned, func(id *Identity) string { return "5;chunk-signature=xyz\r\nhello\r\n" }},
		{"wrong extension name", streamingSigned, func(id *Identity) string { return "5;sig=" + strings.Repeat("0", 64) + "\r\nhello\r\n" }},
		{"truncated header", streamingSigned, func(id *Identity) string { return "5;chunk-signature=" }},
		{"empty body", streamingSigned, func(id *Identity) string { return "" }},
		{"truncated data", streamingSigned, func(id *Identity) string { return sigLine(id) + "hel" }},
		{"missing crlf", streamingSigned, func(id *Identity) string { return sigLine(id) + "helloXX" }},
		{"short crlf", streamingSigned, func(id *Identity) string { return sigLine(id) + "hello\r" }},
		{"line too long", streamingUnsignedTrailer, func(id *Identity) string { return strings.Repeat("1", chunkReaderBufferSize+1) + "\r\n" }},
		{"bad trailer line", streamingUnsignedTrailer, func(id *Identity) string { return "0\r\nnocolon\r\n\r\n" }},
		{"final chunk missing crlf", streamingSigned, func(id *Identity) string {
			return fmt.Sprintf("0;chunk-signature=%s\r\n", newEncoder(id).chunkSig(nil))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := streamingIdentity(t, c.mode)
			cr := NewChunkedReader(strings.NewReader(c.body(id)), id)
			_, err := io.ReadAll(cr)
			wantErr(t, err, ErrMalformed)
		})
	}
}

func TestChunkedReaderUnderlyingError(t *testing.T) {
	id := streamingIdentity(t, streamingSigned)
	body := newEncoder(id).encode(sampleChunks, nil)
	boom := errors.New("connection reset")
	cr := NewChunkedReader(io.MultiReader(bytes.NewReader(body[:1500]), iotest.ErrReader(boom)), id)
	_, err := io.ReadAll(cr)
	wantErr(t, err, boom)
}
