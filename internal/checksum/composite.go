package checksum

import (
	"strconv"

	"github.com/go-faster/errors"
)

// CompositeOf computes a multipart object's COMPOSITE checksum: the digest of
// the concatenated *raw part digests*, with the part count appended after a
// dash.
//
// The "-N" suffix is not decoration. It is the only thing in the value that
// says "this is not a digest of the object's bytes" — a client that fed the
// object back through the same algorithm would get something else entirely,
// and the suffix is what stops that being a mystery.
//
// Parts must be in ascending part-number order, which is the order they are
// concatenated in and therefore the order the digest depends on.
func CompositeOf(a Algorithm, partDigests [][]byte) (string, error) {
	h, err := a.New()
	if err != nil {
		return "", err
	}

	for _, d := range partDigests {
		if _, err := h.Write(d); err != nil {
			return "", errors.Wrap(err, "hash part digest")
		}
	}

	return Encode(h.Sum(nil)) + "-" + strconv.Itoa(len(partDigests)), nil
}
