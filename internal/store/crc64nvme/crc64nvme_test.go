package crc64nvme

import (
	"encoding/base64"
	"testing"
)

func TestKnownVectors(t *testing.T) {
	// Check value from the CRC catalogue (reveng): "123456789" -> 0xae8b14860a799888.
	if got := Checksum([]byte("123456789")); got != 0xae8b14860a799888 {
		t.Fatalf("got %#x", got)
	}
	if Checksum(nil) != 0 {
		t.Fatal("empty must be 0")
	}
	// Streaming equals one-shot.
	h := New()
	_, _ = h.Write([]byte("12345"))
	_, _ = h.Write([]byte("6789"))
	if h.Sum64() != 0xae8b14860a799888 {
		t.Fatal("streaming mismatch")
	}
	if base64.StdEncoding.EncodeToString(h.Sum(nil)) != "rosUhgp5mIg=" {
		t.Fatalf("base64 form: %s", base64.StdEncoding.EncodeToString(h.Sum(nil)))
	}
	h.Reset()
	if h.Sum64() != 0 || h.Size() != 8 || h.BlockSize() != 1 {
		t.Fatal("reset/size")
	}
}
