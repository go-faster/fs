package storagefs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagefs"
)

// The digests here are the ones ceph/s3-tests asserts, for three parts of 5 MiB
// filled with 'A', 'B' and 'C'. Taking them from the suite rather than from our
// own implementation is the point: a composition that agreed only with itself
// would pass every test we wrote and none that AWS did.
const (
	partA = "275VF5loJr1YYawit0XSHREhkFXYkkPKGuoK0x9VKxI="
	partB = "mrHwOfjTL5Zwfj74F05HOQGLdUb7E5szdCbxgUSq6NM="
	partC = "Vw7oB/nKQ5xWb3hNgbyfkvDiivl+U+/Dft48nfJfDow="

	compositeABC = "uWBwpe1dxI4Vw8Gf0X9ynOdw/SS6VBzfWm9giiv1sf4=-3"
)

const partSize = 5 * 1024 * 1024

// threeParts uploads the suite's three bodies and returns the completed parts.
func threeParts(t *testing.T, s *storagefs.Storage, uploadID string) []fs.CompletedPart {
	t.Helper()

	ctx := context.Background()
	out := make([]fs.CompletedPart, 0, 3)

	for i, fill := range []string{"A", "B", "C"} {
		body := strings.Repeat(fill, partSize)

		part, err := s.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket: "bucket-a", Key: "big", UploadID: uploadID,
			PartNumber: i + 1,
			Reader:     strings.NewReader(body),
			Size:       int64(len(body)),
		})
		require.NoError(t, err)

		out = append(out, fs.CompletedPart{
			PartNumber: i + 1, ETag: part.ETag, Checksum: part.Checksum,
		})
	}

	return out
}

// TestMultipartComposesTheChecksumAWSDoes is the acceptance for #120's
// remaining half: a completed multipart object carries a digest *of its part
// digests*, with the part count after a dash.
//
// That shape is what lets a client verify an object it never held whole — it
// uploaded the parts, it knows their digests, and it can compute the same
// composition. A digest of the assembled body would be a number it could only
// get by downloading what it had just sent.
func TestMultipartComposesTheChecksumAWSDoes(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big", ChecksumAlgorithm: "SHA256", ChecksumType: "COMPOSITE",
	})
	require.NoError(t, err)

	parts := threeParts(t, s, up.UploadID)

	assert.Equal(t, partA, parts[0].Checksum, "part 1 is not what the suite computed")
	assert.Equal(t, partB, parts[1].Checksum)
	assert.Equal(t, partC, parts[2].Checksum)

	done, err := s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big", UploadID: up.UploadID,
		Parts: parts, Checksum: compositeABC,
	})
	require.NoError(t, err)

	assert.Equal(t, compositeABC, done.Checksum)
	assert.Equal(t, "COMPOSITE", done.ChecksumType)
	assert.Equal(t, "SHA256", done.ChecksumAlgorithm)
}

// TestAPartThatIsNotWhatItClaimsIsRefused: a part is verified as it arrives,
// like a single PUT, so a completion can never name one that was stored wrong.
func TestAPartThatIsNotWhatItClaimsIsRefused(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big", ChecksumAlgorithm: "SHA256",
	})
	require.NoError(t, err)

	_, err = s.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: "bucket-a", Key: "big", UploadID: up.UploadID, PartNumber: 1,
		Reader: strings.NewReader(strings.Repeat("A", partSize)), Size: partSize,
		Checksum: partB, // the digest of a different body
	})
	require.ErrorIs(t, err, fs.ErrBadDigest)
}

// TestACompletionThatDisagreesWithItsPartsIsRefused: the client's composite is
// a claim, and the parts are the evidence.
func TestACompletionThatDisagreesWithItsPartsIsRefused(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big", ChecksumAlgorithm: "SHA256",
	})
	require.NoError(t, err)

	parts := threeParts(t, s, up.UploadID)

	_, err = s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big", UploadID: up.UploadID,
		Parts: parts, Checksum: partA + "-3", // a well-formed digest of the wrong thing
	})
	require.ErrorIs(t, err, fs.ErrBadDigest)
}

// TestAnUploadWithNoAlgorithmComposesNothing: multipart without a checksum is
// the ordinary case and must stay free of one.
func TestAnUploadWithNoAlgorithmComposesNothing(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big",
	})
	require.NoError(t, err)

	parts := threeParts(t, s, up.UploadID)

	for _, p := range parts {
		assert.Empty(t, p.Checksum)
	}

	done, err := s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big", UploadID: up.UploadID, Parts: parts,
	})
	require.NoError(t, err)

	assert.Empty(t, done.Checksum)
	assert.Empty(t, done.ChecksumType)
}

// TestFullObjectIsRefusedForAlgorithmsThatCannotCompose: only the CRCs compose
// linearly, so only they can carry a digest of the whole body across parts.
// Asking SHA-256 for one is asking for a number nothing can produce.
func TestFullObjectIsRefusedForAlgorithmsThatCannotCompose(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	_, err = s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big",
		ChecksumAlgorithm: "SHA256", ChecksumType: "FULL_OBJECT",
	})
	require.Error(t, err)

	// And the CRCs are allowed it.
	_, err = s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big2",
		ChecksumAlgorithm: "CRC32", ChecksumType: "FULL_OBJECT",
	})
	require.NoError(t, err)
}

// TestFullObjectDigestsTheBodyNotThePartDigests is the distinction the two
// checksum types actually make, and the one that is easy to get wrong by
// treating the type as a formatting choice.
//
// A COMPOSITE value is a digest *of the part digests*, and says so with its
// "-N" suffix. A FULL_OBJECT value is the digest of the assembled body — the
// same number a client gets by hashing what it downloads — and carries no
// suffix. Composing the parts for a FULL_OBJECT upload produces a different
// number and an unwanted suffix, which is precisely what the CRC tests in
// ceph/s3-tests catch.
func TestFullObjectDigestsTheBodyNotThePartDigests(t *testing.T) {
	ctx := context.Background()

	s, err := storagefs.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big",
		ChecksumAlgorithm: "CRC32", ChecksumType: "FULL_OBJECT",
	})
	require.NoError(t, err)

	parts := threeParts(t, s, up.UploadID)

	done, err := s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "bucket-a", Key: "big", UploadID: up.UploadID, Parts: parts,
	})
	require.NoError(t, err)

	// ceph/s3-tests' own expected value for CRC32 over the three bodies.
	assert.Equal(t, "WgDhBQ==", done.Checksum)
	assert.Equal(t, "FULL_OBJECT", done.ChecksumType)
	assert.NotContains(t, done.Checksum, "-", "a full-object digest carried a part count")
}
