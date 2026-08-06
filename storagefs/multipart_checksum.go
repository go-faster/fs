package storagefs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/checksum"
)

// uploadChecksum settles what an upload's parts will be digested with.
//
// Both are optional and either can imply the other: an algorithm with no type
// takes the algorithm's default, and a type with no algorithm is a client
// asking for a shape without saying of what, which is nothing to compute.
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

	t := checksum.Type(kind)
	switch t {
	case checksum.Composite:
	case checksum.FullObject:
		// Only the CRCs compose linearly, so only they can carry a digest of
		// the whole body across parts. Asking SHA-256 for one would be asking
		// for a number that cannot be assembled from the parts at all.
		if !a.SupportsFullObject() {
			return "", "", errors.Wrapf(fs.ErrInvalidDigest,
				"checksum type %s is not available for %s", t, a)
		}
	default:
		return "", "", errors.Wrapf(fs.ErrInvalidDigest, "unknown checksum type %q", kind)
	}

	return a, t, nil
}

// partChecksumPath is where a part's own digest is kept: beside the part, under
// a dot-prefixed name so it is never mistaken for one.
//
// Kept on disk rather than in the upload's metadata because parts arrive
// concurrently, and a shared metadata file rewritten per part is a lost update
// waiting for two clients that upload at the same time — which is the ordinary
// way a large upload is done.
func partChecksumPath(uploadPath string, partNumber int) string {
	return filepath.Join(uploadPath, "."+strconv.Itoa(partNumber)+".cksum")
}

// partChecksum is what a part carried, as stored beside it.
type partChecksum struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}

// recordPartChecksum stores a part's digest next to the part.
func recordPartChecksum(uploadPath string, partNumber int, a checksum.Algorithm, digest string) error {
	if digest == "" {
		return nil
	}

	raw, err := json.Marshal(partChecksum{Algorithm: string(a), Digest: digest})
	if err != nil {
		return errors.Wrap(err, "marshal part checksum")
	}

	if err := os.WriteFile(partChecksumPath(uploadPath, partNumber), raw, 0o600); err != nil {
		return errors.Wrap(err, "record part checksum")
	}

	return nil
}

// loadPartChecksum reads back what a part carried, empty when it carried none.
func loadPartChecksum(uploadPath string, partNumber int) string {
	raw, err := os.ReadFile(partChecksumPath(uploadPath, partNumber))
	if err != nil {
		return ""
	}

	var got partChecksum
	if err := json.Unmarshal(raw, &got); err != nil {
		return ""
	}

	return got.Digest
}

// completionChecksum is the completed object's digest, composed from its parts.
//
// # Composed, never taken over the assembled body
//
// That is the rule that makes a multipart checksum verifiable by a client that
// never held the whole object: it uploaded parts, it knows their digests, and it
// can compute the same composition. A digest of the assembled bytes would be a
// number the client has no way to arrive at without downloading what it just
// uploaded.
//
// The exception is a CRC asked for as FULL_OBJECT, where the algorithm composes
// linearly and the combined value really is the digest of the whole body.
//
// # The client's claim is checked, not trusted
//
// A completion may name the digest it expects. It is compared against what the
// parts actually add up to, and a disagreement is BadDigest — the same answer a
// single PUT gives, for the same reason.
func completionChecksum(
	meta *multipartMetadata,
	partDigests []string,
	claimed string,
) (string, checksum.Type, error) {
	if meta.ChecksumAlgorithm == "" {
		return "", "", nil
	}

	a, err := checksum.Parse(meta.ChecksumAlgorithm)
	if err != nil {
		return "", "", errors.Wrap(fs.ErrInvalidDigest, err.Error())
	}

	kind := checksum.Type(meta.ChecksumType)
	if kind == "" {
		kind = a.DefaultType()
	}

	raw := make([][]byte, 0, len(partDigests))

	for _, digest := range partDigests {
		if digest == "" {
			// A part with no digest cannot be composed with the others, and a
			// composition missing one is not the object's checksum. Reported
			// rather than papered over with a partial answer.
			return "", "", errors.Wrap(fs.ErrInvalidPart,
				"a part of this upload carries no checksum")
		}

		decoded, err := a.Decode(digest)
		if err != nil {
			return "", "", errors.Wrap(fs.ErrBadDigest, err.Error())
		}

		raw = append(raw, decoded)
	}

	composed, err := checksum.CompositeOf(a, raw)
	if err != nil {
		return "", "", errors.Wrap(fs.ErrBadDigest, err.Error())
	}

	if claimed != "" && claimed != composed {
		return "", "", fs.ErrBadDigest
	}

	return composed, kind, nil
}
