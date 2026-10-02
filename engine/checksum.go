package engine

import (
	"hash"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/checksum"
)

// The client-visible checksum (x-amz-checksum-*) is a digest of the bytes the
// client sent — the plaintext — taken where the ETag is. A single PUT's is of
// the body. A multipart object's is composed from its parts' digests, which a
// client that only ever held parts can compute too, or for a CRC asked for as
// FULL_OBJECT, combined from them into the digest of the whole body: CRCs are
// linear, so that needs the parts' digests and lengths and not their bytes.

// objectChecksum computes a checksum as a body streams past.
type objectChecksum struct {
	algorithm checksum.Algorithm
	h         hash.Hash
}

// newChecksum returns the accumulator for the named algorithm, or one that
// discards writes when none was asked for.
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

// value is the digest, empty when none was asked for.
func (c *objectChecksum) value() string {
	if c.h == nil {
		return ""
	}

	return checksum.Encode(c.h.Sum(nil))
}

// verify compares what the client claimed with what arrived. A claim that is
// not even a digest of the algorithm is the same BadDigest: either way the
// object was not stored.
func (c *objectChecksum) verify(claimed string) error {
	if claimed == "" || c.h == nil {
		return nil
	}

	if _, err := c.algorithm.Decode(claimed); err != nil || claimed != c.value() {
		return fs.ErrBadDigest
	}

	return nil
}

// uploadChecksum settles an upload's algorithm and type: a type without an
// algorithm is nothing to compute, an algorithm without a type takes its
// default, and FULL_OBJECT is only for algorithms that combine.
func uploadChecksum(algorithm, kind string) (checksum.Algorithm, checksum.Type, error) {
	if algorithm == "" {
		return "", "", nil
	}

	a, err := checksum.Parse(algorithm)
	if err != nil {
		return "", "", errors.Wrap(fs.ErrInvalidDigest, err.Error())
	}

	if kind == "" {
		return a, a.DefaultType(), nil
	}

	switch t := checksum.Type(kind); t {
	case checksum.Composite:
		return a, t, nil
	case checksum.FullObject:
		if !a.SupportsFullObject() {
			return "", "", errors.Wrapf(fs.ErrInvalidDigest, "checksum type %s is not available for %s", t, a)
		}

		return a, t, nil
	default:
		return "", "", errors.Wrapf(fs.ErrInvalidDigest, "unknown checksum type %q", kind)
	}
}

// partDigest is one part's checksum and length, as completion composes them.
type partDigest struct {
	digest string
	size   int64
}

// completionChecksum composes an upload's parts into the object's checksum and
// checks it against the client's claim.
func completionChecksum(algorithm, kind string, parts []partDigest, claimed string) (string, error) {
	if algorithm == "" {
		return "", nil
	}

	a, err := checksum.Parse(algorithm)
	if err != nil {
		return "", errors.Wrap(fs.ErrInvalidDigest, err.Error())
	}

	raw := make([][]byte, 0, len(parts))

	for _, p := range parts {
		if p.digest == "" {
			// A part with no digest cannot be composed with the others, and a
			// composition missing one is not the object's checksum.
			return "", errors.Wrap(fs.ErrInvalidPart, "a part of this upload carries no checksum")
		}

		d, err := a.Decode(p.digest)
		if err != nil {
			return "", errors.Wrap(fs.ErrBadDigest, err.Error())
		}

		raw = append(raw, d)
	}

	var out string

	if checksum.Type(kind) == checksum.FullObject {
		var whole []byte

		for i, d := range raw {
			if i == 0 {
				whole = d

				continue
			}

			if whole, err = a.Combine(whole, d, parts[i].size); err != nil {
				return "", errors.Wrap(fs.ErrInvalidDigest, err.Error())
			}
		}

		out = checksum.Encode(whole)
	} else {
		if out, err = checksum.CompositeOf(a, raw); err != nil {
			return "", errors.Wrap(fs.ErrBadDigest, err.Error())
		}
	}

	if claimed != "" && claimed != out {
		return "", fs.ErrBadDigest
	}

	return out, nil
}
