package auth

import (
	"context"
	"net/http/httptest"
	"testing"
)

func FuzzVerifyAuthorizationHeader(f *testing.F) {
	f.Add("AWS4-HMAC-SHA256 Credential=AKID/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-date, Signature=abc")
	f.Add("AWS AKID:sig")
	f.Add("AWS4-HMAC-SHA256 Credential=,SignedHeaders=,Signature=")
	f.Add("")
	v := &Verifier{Lookup: func(context.Context, string) (string, error) { return "secret", nil }}
	f.Fuzz(func(t *testing.T, h string) {
		r := httptest.NewRequest("GET", "http://h/b/k?x=1", nil)
		r.Header.Set("Authorization", h)
		r.Header.Set("x-amz-date", "20130524T000000Z")
		_, _ = v.Verify(context.Background(), r)
	})
}

func FuzzChunkedReader(f *testing.F) {
	f.Add([]byte("5;chunk-signature=abcd\r\nhello\r\n0;chunk-signature=ef\r\n\r\n"))
	f.Add([]byte("zz\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, body []byte) {
		id := &Identity{Payload: PayloadStreaming, SigningKey: []byte("k"), SeedSignature: "00", Scope: "d/r/s/aws4_request"}
		cr := NewChunkedReader(bytesReader(body), id)
		buf := make([]byte, 64)
		for i := 0; i < 1000; i++ {
			if _, err := cr.Read(buf); err != nil {
				break
			}
		}
	})
}
