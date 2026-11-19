// Package crccombine combines CRCs of concatenated byte sequences without the
// data, for reflected CRC-32 and CRC-64 polynomials. It is the standard
// zlib crc32_combine construction generalised with GF(2) matrix exponentiation.
package crccombine

// Poly describes a reflected CRC.
type Poly struct {
	Bits   int    // 32 or 64
	Poly   uint64 // reflected polynomial
	Init   uint64 // initial register value (after reflection), e.g. all ones
	XorOut uint64
}

// Well-known polynomials.
var (
	CRC32IEEE = Poly{Bits: 32, Poly: 0xedb88320, Init: 0xffffffff, XorOut: 0xffffffff}
	CRC32C    = Poly{Bits: 32, Poly: 0x82f63b78, Init: 0xffffffff, XorOut: 0xffffffff}
	CRC64NVME = Poly{Bits: 64, Poly: 0x9a6c9329ac4bc9b5, Init: ^uint64(0), XorOut: ^uint64(0)}
)

func (p Poly) mask() uint64 {
	if p.Bits == 64 {
		return ^uint64(0)
	}
	return (1 << uint(p.Bits)) - 1
}

// gf2MatrixTimes multiplies matrix by vector over GF(2).
func gf2MatrixTimes(mat []uint64, vec uint64) uint64 {
	var sum uint64
	for i := 0; vec != 0; i++ {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
		vec >>= 1
	}
	return sum
}

func gf2MatrixSquare(square, mat []uint64) {
	for n := range mat {
		square[n] = gf2MatrixTimes(mat, mat[n])
	}
}

// Combine returns the CRC of A||B given crc(A), crc(B) and len(B).
func (p Poly) Combine(crc1, crc2 uint64, len2 int64) uint64 {
	if len2 <= 0 {
		return crc1
	}
	n := p.Bits
	even := make([]uint64, n)
	odd := make([]uint64, n)
	// Operator for one zero bit.
	odd[0] = p.Poly
	row := uint64(1)
	for i := 1; i < n; i++ {
		odd[i] = row
		row <<= 1
	}
	gf2MatrixSquare(even, odd) // two zero bits
	gf2MatrixSquare(odd, even) // four zero bits

	// Because Init equals XorOut for every supported polynomial, the init
	// and final-xor terms cancel and the shift can be applied to the final
	// CRC value directly (the zlib construction).
	mask := p.mask()
	crc1 &= mask
	crc2 &= mask
	for {
		gf2MatrixSquare(even, odd)
		if len2&1 != 0 {
			crc1 = gf2MatrixTimes(even, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
		gf2MatrixSquare(odd, even)
		if len2&1 != 0 {
			crc1 = gf2MatrixTimes(odd, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
	}
	return (crc1 ^ crc2) & mask
}
