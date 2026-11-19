// Package crc64nvme implements the CRC-64/NVME checksum used by AWS S3
// (polynomial 0xad93d23594c93659, reflected, init and xorout all ones).
package crc64nvme

import "hash"

const poly = 0x9a6c9329ac4bc9b5 // reflected form of 0xad93d23594c93659

var table = func() [256]uint64 {
	var t [256]uint64
	for i := 0; i < 256; i++ {
		crc := uint64(i)
		for j := 0; j < 8; j++ {
			if crc&1 == 1 {
				crc = (crc >> 1) ^ poly
			} else {
				crc >>= 1
			}
		}
		t[i] = crc
	}
	return t
}()

// Update returns the CRC after processing p starting from crc.
func Update(crc uint64, p []byte) uint64 {
	crc = ^crc
	for _, b := range p {
		crc = table[byte(crc)^b] ^ (crc >> 8)
	}
	return ^crc
}

// Checksum returns the CRC-64/NVME of data.
func Checksum(data []byte) uint64 { return Update(0, data) }

type digest struct{ crc uint64 }

// New returns a hash.Hash64 computing CRC-64/NVME.
func New() hash.Hash64 { return &digest{} }

func (d *digest) Write(p []byte) (int, error) { d.crc = Update(d.crc, p); return len(p), nil }
func (d *digest) Sum64() uint64               { return d.crc }
func (d *digest) Reset()                      { d.crc = 0 }
func (d *digest) Size() int                   { return 8 }
func (d *digest) BlockSize() int              { return 1 }
func (d *digest) Sum(b []byte) []byte {
	s := d.Sum64()
	return append(b, byte(s>>56), byte(s>>48), byte(s>>40), byte(s>>32), byte(s>>24), byte(s>>16), byte(s>>8), byte(s))
}
