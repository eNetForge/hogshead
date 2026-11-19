// Package ids generates identifiers used across the system.
package ids

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return b
}

// VersionID returns a 32-character version id. S3 version ids are opaque;
// ours are URL safe and never equal to "null".
func VersionID() string { return b32.EncodeToString(random(20)) }

// ServerID returns the random identity of a server.
func ServerID() string { return "s-" + hex.EncodeToString(random(8)) }

// TransferID returns a short id for a transfer job.
func TransferID() string { return "t-" + hex.EncodeToString(random(6)) }

// UploadID returns a multipart upload id.
func UploadID() string { return b32.EncodeToString(random(30)) }

// UserID returns a user id.
func UserID() string { return "u-" + hex.EncodeToString(random(8)) }

// Password returns a random 20-character password from an unambiguous alphabet.
func Password() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := random(20)
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = alphabet[int(c)%len(alphabet)]
	}
	return string(out)
}

// AccessKeyID returns a 20-character AWS-style access key id.
func AccessKeyID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	b := random(16)
	var sb strings.Builder
	sb.WriteString("MNON")
	for _, c := range b {
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return sb.String()
}

// SecretKey returns a 40-character secret.
func SecretKey() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := random(40)
	out := make([]byte, 40)
	for i, c := range b {
		out[i] = alphabet[int(c)%len(alphabet)]
	}
	return string(out)
}

// RequestID returns a 16-character request id.
func RequestID() string { return strings.ToUpper(hex.EncodeToString(random(8))) }

// Token returns a random hex token of n bytes.
func Token(n int) string { return hex.EncodeToString(random(n)) }
