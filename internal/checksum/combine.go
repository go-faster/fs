package checksum

import (
	"encoding/binary"

	"github.com/go-faster/errors"
)

// Combine returns the digest of two bodies concatenated, from their digests
// and the second body's length. It exists for FULL_OBJECT multipart checksums:
// the whole object's CRC, from its parts' CRCs, without reading the object.
//
// A CRC is linear over GF(2): appending n bytes to a message multiplies its
// CRC by x^(8n) modulo the polynomial, and the CRC of the concatenation is that
// product XOR the second CRC. The multiplication is done by repeatedly
// squaring a matrix — zlib's crc32_combine, for any width up to 64.
func (a Algorithm) Combine(first, second []byte, secondLen int64) ([]byte, error) {
	var (
		poly  uint64
		width int
	)

	switch a {
	case CRC32:
		poly, width = 0xedb88320, 32
	case CRC32C:
		poly, width = 0x82f63b78, 32
	case CRC64NVME:
		poly, width = 0x9a6c9329ac4bc9b5, 64
	default:
		return nil, errors.Errorf("%s digests do not combine", a)
	}

	if len(first) != width/8 || len(second) != width/8 {
		return nil, errors.Errorf("%s digests are %d bytes", a, width/8)
	}

	var c1, c2 uint64
	if width == 32 {
		c1, c2 = uint64(binary.BigEndian.Uint32(first)), uint64(binary.BigEndian.Uint32(second))
	} else {
		c1, c2 = binary.BigEndian.Uint64(first), binary.BigEndian.Uint64(second)
	}

	out := combine(c1, c2, secondLen, poly, width)

	if width == 32 {
		return binary.BigEndian.AppendUint32(nil, uint32(out)), nil //nolint:gosec // A 32-bit CRC fits.
	}

	return binary.BigEndian.AppendUint64(nil, out), nil
}

// combine is zlib's crc32_combine generalized: odd and even are the operators
// that append one and two zero bits, squared up to the length of the second
// message one bit of it at a time.
func combine(crc1, crc2 uint64, len2 int64, poly uint64, width int) uint64 {
	if len2 <= 0 {
		return crc1 ^ crc2
	}

	even := make([]uint64, width)
	odd := make([]uint64, width)

	odd[0] = poly
	row := uint64(1)

	for n := 1; n < width; n++ {
		odd[n] = row
		row <<= 1
	}

	square(even, odd) // Two zero bits.
	square(odd, even) // Four.

	for {
		square(even, odd) // One more zero byte's worth at each step.

		if len2&1 != 0 {
			crc1 = times(even, crc1)
		}

		len2 >>= 1
		if len2 == 0 {
			break
		}

		square(odd, even)

		if len2&1 != 0 {
			crc1 = times(odd, crc1)
		}

		len2 >>= 1
		if len2 == 0 {
			break
		}
	}

	return crc1 ^ crc2
}

func times(mat []uint64, vec uint64) uint64 {
	var sum uint64

	for i := 0; vec != 0; i, vec = i+1, vec>>1 {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
	}

	return sum
}

func square(dst, mat []uint64) {
	for n := range mat {
		dst[n] = times(mat, mat[n])
	}
}
