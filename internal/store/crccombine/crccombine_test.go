package crccombine

import (
	"crypto/rand"
	"hash/crc32"
	"testing"

	"github.com/enetforge/hogshead/internal/store/crc64nvme"
)

func TestCombine(t *testing.T) {
	for _, sizes := range [][2]int{{0, 0}, {1, 1}, {7, 3}, {100, 0}, {0, 100}, {1000, 777}, {4096, 4096}, {65536, 12345}} {
		a := make([]byte, sizes[0])
		b := make([]byte, sizes[1])
		_, _ = rand.Read(a)
		_, _ = rand.Read(b)
		ab := append(append([]byte{}, a...), b...)

		ieee := CRC32IEEE.Combine(uint64(crc32.ChecksumIEEE(a)), uint64(crc32.ChecksumIEEE(b)), int64(len(b)))
		if ieee != uint64(crc32.ChecksumIEEE(ab)) {
			t.Fatalf("ieee %v", sizes)
		}
		tab := crc32.MakeTable(crc32.Castagnoli)
		c := CRC32C.Combine(uint64(crc32.Checksum(a, tab)), uint64(crc32.Checksum(b, tab)), int64(len(b)))
		if c != uint64(crc32.Checksum(ab, tab)) {
			t.Fatalf("castagnoli %v", sizes)
		}
		n := CRC64NVME.Combine(crc64nvme.Checksum(a), crc64nvme.Checksum(b), int64(len(b)))
		if n != crc64nvme.Checksum(ab) {
			t.Fatalf("nvme %v: %x vs %x", sizes, n, crc64nvme.Checksum(ab))
		}
	}
}
