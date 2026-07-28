package checksum

import (
	"strconv"
	"strings"

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

// SplitComposite separates a composite value into its digest and part count.
// ok is false when the value carries no suffix, which is what a FULL_OBJECT
// checksum looks like.
func SplitComposite(v string) (digest string, parts int, ok bool) {
	i := strings.LastIndexByte(v, '-')
	if i < 0 {
		return v, 0, false
	}

	n, err := strconv.Atoi(v[i+1:])
	if err != nil || n <= 0 {
		return v, 0, false
	}

	return v[:i], n, true
}
