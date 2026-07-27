// Package checksum implements the client-visible checksums S3 exposes as
// x-amz-checksum-*.
//
// This is a *second* checksum, distinct from the content MD5 behind ETag and
// from the integrity checksum the scrubber uses. It has its own algorithms,
// its own negotiation, and its own composition rule for multipart objects, and
// conflating it with either of the others is the mistake this package exists
// to make hard: the ETag is a property of how the object was stored, while
// this is a digest the *client* chose, sent, and expects back unchanged.
package checksum

import (
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strings"

	"crypto/sha1" //nolint:gosec // SHA-1 is one of the algorithms S3 defines here.
	"crypto/sha256"

	"github.com/go-faster/errors"
)

// Algorithm is one of the checksum algorithms S3 defines. The zero value means
// no checksum was requested.
type Algorithm string

// The algorithms S3 accepts, spelled as they appear on the wire.
const (
	CRC32     Algorithm = "CRC32"
	CRC32C    Algorithm = "CRC32C"
	CRC64NVME Algorithm = "CRC64NVME"
	SHA1      Algorithm = "SHA1"
	SHA256    Algorithm = "SHA256"
)

// Type is how a multipart object's checksum relates to its parts.
type Type string

const (
	// Composite is a digest *of the part digests*, which is why its value
	// carries a "-N" suffix naming the part count: it is not a digest of the
	// object's bytes and must not be mistaken for one.
	Composite Type = "COMPOSITE"
	// FullObject is a digest of the whole body, as if it had been written by a
	// single PUT. Only the CRCs support it, because only they compose: two
	// CRCs can be combined into the CRC of the concatenation, and two SHAs
	// cannot.
	FullObject Type = "FULL_OBJECT"
)

// crc64NVMETable is the CRC-64/NVME polynomial, which is neither of the two
// hash/crc64 ships. S3 added this algorithm because it is the one NVMe drives
// compute in hardware, so a client can forward a digest its disk already made.
//
//nolint:gochecknoglobals // A CRC table is immutable and expensive to rebuild.
var crc64NVMETable = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// Parse maps a wire value to an Algorithm, case-insensitively as S3 does.
func Parse(s string) (Algorithm, error) {
	switch Algorithm(strings.ToUpper(strings.TrimSpace(s))) {
	case CRC32:
		return CRC32, nil
	case CRC32C:
		return CRC32C, nil
	case CRC64NVME:
		return CRC64NVME, nil
	case SHA1:
		return SHA1, nil
	case SHA256:
		return SHA256, nil
	case "":
		return "", nil
	default:
		return "", errors.Errorf("unknown checksum algorithm %q", s)
	}
}

// Header is the request/response header carrying this algorithm's digest.
func (a Algorithm) Header() string {
	if a == "" {
		return ""
	}

	return "x-amz-checksum-" + strings.ToLower(string(a))
}

// SupportsFullObject reports whether the algorithm can produce a FULL_OBJECT
// multipart checksum. Only the CRCs can: combining two of them yields the CRC
// of the concatenation, which is what makes a whole-object digest computable
// from parts without re-reading the object.
func (a Algorithm) SupportsFullObject() bool {
	switch a {
	case CRC32, CRC32C, CRC64NVME:
		return true
	case SHA1, SHA256:
		return false
	default:
		return false
	}
}

// DefaultType is the checksum type S3 uses when a multipart upload names an
// algorithm but no type.
func (a Algorithm) DefaultType() Type {
	if a.SupportsFullObject() {
		return FullObject
	}

	return Composite
}

// New returns a running hash for the algorithm.
func (a Algorithm) New() (hash.Hash, error) {
	switch a {
	case CRC32:
		return crc32.NewIEEE(), nil
	case CRC32C:
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), nil
	case CRC64NVME:
		return crc64.New(crc64NVMETable), nil
	case SHA1:
		return sha1.New(), nil //nolint:gosec // One of the algorithms S3 defines.
	case SHA256:
		return sha256.New(), nil
	default:
		return nil, errors.Errorf("no hash for checksum algorithm %q", a)
	}
}

// Encode renders a digest the way S3 does: standard base64.
func Encode(sum []byte) string {
	return base64.StdEncoding.EncodeToString(sum)
}

// Decode reads a digest a client sent, and reports whether it is well-formed
// for the algorithm.
//
// A digest of the wrong length is rejected here rather than compared and found
// unequal, so "you sent nonsense" and "your bytes did not match" stay
// distinguishable.
func (a Algorithm) Decode(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.Wrapf(err, "decode %s checksum", a)
	}

	want, err := a.Size()
	if err != nil {
		return nil, err
	}

	if len(raw) != want {
		return nil, errors.Errorf("%s checksum is %d bytes, want %d", a, len(raw), want)
	}

	return raw, nil
}

// Size is the digest length in bytes.
func (a Algorithm) Size() (int, error) {
	switch a {
	case CRC32, CRC32C:
		return 4, nil
	case CRC64NVME:
		return 8, nil
	case SHA1:
		return 20, nil
	case SHA256:
		return 32, nil
	default:
		return 0, errors.Errorf("no size for checksum algorithm %q", a)
	}
}
