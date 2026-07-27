package storagefs

import (
	"hash"
	"io"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/checksum"
)

// objectChecksum computes the client-visible checksum as a body streams past,
// and holds what has to be said about it afterwards.
//
// It is a writer so it can join the same MultiWriter the ETag uses: the client
// asked for a digest of the bytes it sent, which is exactly the point in the
// pipeline the ETag is taken from, and computing it anywhere else would digest
// the stored form instead.
type objectChecksum struct {
	algorithm checksum.Algorithm
	h         hash.Hash
	sum       string
}

// newChecksum returns the accumulator for the named algorithm; a zero-value
// one that discards writes when no checksum was asked for.
func newChecksum(algorithm string) (*objectChecksum, error) {
	if algorithm == "" {
		return &objectChecksum{}, nil
	}

	a, err := checksum.Parse(algorithm)
	if err != nil {
		return nil, errors.Wrap(fs.ErrInvalidDigest, err.Error())
	}

	h, err := a.New()
	if err != nil {
		return nil, errors.Wrap(fs.ErrInvalidDigest, err.Error())
	}

	return &objectChecksum{algorithm: a, h: h}, nil
}

func (c *objectChecksum) Write(p []byte) (int, error) {
	if c.h == nil {
		return len(p), nil
	}

	return c.h.Write(p) //nolint:wrapcheck // hash.Hash never errors.
}

// value is the computed digest, empty when none was asked for.
func (c *objectChecksum) value() string {
	if c.h == nil {
		return ""
	}

	if c.sum == "" {
		c.sum = checksum.Encode(c.h.Sum(nil))
	}

	return c.sum
}

// verify compares the digest the client claimed against what actually arrived.
//
// A claim that is not a well-formed digest for the algorithm is refused too,
// and as the same error: from the client's side both mean "the object was not
// stored", and inventing a second code for it would be a distinction S3 does
// not make.
func (c *objectChecksum) verify(claimed string) error {
	if claimed == "" || c.h == nil {
		return nil
	}

	if _, err := c.algorithm.Decode(claimed); err != nil {
		return fs.ErrBadDigest
	}

	if claimed != c.value() {
		return fs.ErrBadDigest
	}

	return nil
}

// record stores the checksum on the object's sidecar.
func (c *objectChecksum) record(sc *sidecar) {
	if c.h == nil {
		return
	}

	sc.ChecksumAlgorithm = string(c.algorithm)
	sc.ClientChecksum = c.value()
	// A single PUT is one whole object, so its digest is of the body itself.
	sc.ChecksumType = string(checksum.FullObject)
}

var _ io.Writer = (*objectChecksum)(nil)
