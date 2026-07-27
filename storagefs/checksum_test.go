package storagefs

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func checksumStore(t *testing.T) *Storage {
	t.Helper()

	s, err := New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(t.Context(), "bucket"))

	return s
}

func putWithChecksum(t *testing.T, s *Storage, algorithm, digest string, body []byte) error {
	t.Helper()

	_, err := s.PutObject(t.Context(), &fs.PutObjectRequest{
		Reader: bytes.NewReader(body), Bucket: "bucket", Key: "k", Size: int64(len(body)),
		ChecksumAlgorithm: algorithm, Checksum: digest,
	})

	return err
}

// TestChecksumStoredAndReported is the round trip: the digest the client sent
// comes back unchanged.
func TestChecksumStoredAndReported(t *testing.T) {
	s := checksumStore(t)
	body := []byte(strings.Repeat("A", 1024))

	// The digest ceph/s3-tests asserts for this body.
	const want = "arcu6553sHVAiX4MjW0j7I7vD4w6R+Gz9Ok0Q9lTa+0="

	require.NoError(t, putWithChecksum(t, s, "SHA256", want, body))

	resp, err := s.GetObject(t.Context(), "bucket", "k")
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	require.Equal(t, "SHA256", resp.ChecksumAlgorithm)
	require.Equal(t, want, resp.Checksum)
	require.Equal(t, "FULL_OBJECT", resp.ChecksumType,
		"a single PUT is one whole object, so its digest is of the body")
}

// TestWrongChecksumRefused is the assertion a round-trip test cannot make: the
// bytes are fine, and the write must still be refused because the client's
// claim about them was wrong.
func TestWrongChecksumRefused(t *testing.T) {
	s := checksumStore(t)
	body := []byte(strings.Repeat("A", 1024))

	// A well-formed SHA256 that is not this body's.
	const wrong = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

	require.ErrorIs(t, putWithChecksum(t, s, "SHA256", wrong, body), fs.ErrBadDigest)

	_, err := s.GetObject(t.Context(), "bucket", "k")
	require.ErrorIs(t, err, fs.ErrObjectNotFound,
		"a refused write must not leave an object behind")
}

// TestMalformedChecksumRefused: a claim that is not a digest at all is
// refused, and as the same error — from the client's side both mean the object
// was not stored.
func TestMalformedChecksumRefused(t *testing.T) {
	s := checksumStore(t)

	require.ErrorIs(t, putWithChecksum(t, s, "SHA256", "bad", []byte("x")), fs.ErrBadDigest)
	require.ErrorIs(t, putWithChecksum(t, s, "SHA256", "Qeh8oXvGiSo=", []byte("x")), fs.ErrBadDigest)
}

// TestChecksumComputedWithoutClaim: an algorithm with no digest means "compute
// and tell me", which is what the SDKs send.
func TestChecksumComputedWithoutClaim(t *testing.T) {
	s := checksumStore(t)
	body := []byte(strings.Repeat("A", 1024))

	resp, err := s.PutObject(t.Context(), &fs.PutObjectRequest{
		Reader: bytes.NewReader(body), Bucket: "bucket", Key: "k", Size: int64(len(body)),
		ChecksumAlgorithm: "SHA256",
	})
	require.NoError(t, err)
	require.Equal(t, "arcu6553sHVAiX4MjW0j7I7vD4w6R+Gz9Ok0Q9lTa+0=", resp.Checksum)
}

// TestNoChecksumWhenNoneAsked: the feature is opt-in, and an object written
// without it reports nothing rather than a computed default.
func TestNoChecksumWhenNoneAsked(t *testing.T) {
	s := checksumStore(t)

	require.NoError(t, putWithChecksum(t, s, "", "", []byte("plain")))

	resp, err := s.GetObject(t.Context(), "bucket", "k")
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	require.Empty(t, resp.ChecksumAlgorithm)
	require.Empty(t, resp.Checksum)
}

func TestUnknownChecksumAlgorithmRefused(t *testing.T) {
	s := checksumStore(t)

	require.Error(t, putWithChecksum(t, s, "MD5", "", []byte("x")))
}
