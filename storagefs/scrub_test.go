package storagefs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// corrupt flips a byte in the object file to simulate bit-rot.
//
//nolint:unparam // bucket kept explicit for readability.
func corrupt(t *testing.T, root, bucket, key string) {
	t.Helper()

	path := filepath.Join(root, bucket, toOSPath(key))

	data, err := os.ReadFile(path) //nolint:gosec // test path.
	require.NoError(t, err)
	require.NotEmpty(t, data)

	data[0] ^= 0xFF
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

//nolint:unparam // bucket kept explicit for readability.
func putContent(t *testing.T, s *Storage, bucket, key string, content []byte) {
	t.Helper()

	_, err := s.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket: bucket, Key: key, Reader: bytes.NewReader(content), Size: int64(len(content)),
	})
	require.NoError(t, err)
}

func TestScrub_Clean(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "b"))
	putContent(t, s, "b", "a.txt", []byte("hello"))
	putContent(t, s, "b", "nested/c.txt", []byte("world"))

	report, err := s.Scrub(ctx, ScrubOptions{})
	require.NoError(t, err)
	require.True(t, report.Healthy())
	require.Equal(t, 2, report.Scanned)
	require.Equal(t, 2, report.OK)
	require.Empty(t, report.Corrupt)
}

func TestScrub_DetectsBitRot(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "b"))
	putContent(t, s, "b", "good.txt", []byte("intact content"))
	putContent(t, s, "b", "rotted.txt", []byte("this will rot"))

	corrupt(t, root, "b", "rotted.txt")

	report, err := s.Scrub(ctx, ScrubOptions{})
	require.NoError(t, err)
	require.False(t, report.Healthy())
	require.Equal(t, 1, report.OK)
	require.Equal(t, []ObjectRef{{Bucket: "b", Key: "rotted.txt"}}, report.Corrupt)
	require.Zero(t, report.Quarantined)
}

func TestScrub_Quarantine(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "b"))
	putContent(t, s, "b", "bad.txt", []byte("corrupt me"))
	corrupt(t, root, "b", "bad.txt")

	report, err := s.Scrub(ctx, ScrubOptions{Quarantine: true})
	require.NoError(t, err)
	require.Len(t, report.Corrupt, 1)
	require.Equal(t, 1, report.Quarantined)

	// The corrupt object is moved aside: no longer served or listed.
	_, err = s.GetObject(ctx, "b", "bad.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)

	objects, err := s.ListObjects(ctx, "b", "")
	require.NoError(t, err)
	require.Empty(t, objects)

	// It lives under the quarantine tree.
	_, err = os.Stat(filepath.Join(root, quarantineSubdir, "b", "bad.txt"))
	require.NoError(t, err)
}

func TestScrub_MultipartChecksum(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "b"))

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "big.bin"})
	require.NoError(t, err)

	p1 := bytes.Repeat([]byte("a"), 6*1024*1024)
	p2 := []byte("tail")

	e1, err := s.UploadPart(ctx, &fs.UploadPartRequest{Bucket: "b", Key: "big.bin", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(p1), Size: int64(len(p1))})
	require.NoError(t, err)
	e2, err := s.UploadPart(ctx, &fs.UploadPartRequest{Bucket: "b", Key: "big.bin", UploadID: up.UploadID, PartNumber: 2, Reader: bytes.NewReader(p2), Size: int64(len(p2))})
	require.NoError(t, err)

	_, err = s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "big.bin", UploadID: up.UploadID,
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: e1.ETag}, {PartNumber: 2, ETag: e2.ETag}},
	})
	require.NoError(t, err)

	// A multipart object (ETag is the "-N" form) is still verifiable via its
	// stored content checksum.
	report, err := s.Scrub(ctx, ScrubOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Scanned)
	require.Equal(t, 1, report.OK)
	require.Zero(t, report.Unverifiable)

	corrupt(t, root, "b", "big.bin")

	report, err = s.Scrub(ctx, ScrubOptions{})
	require.NoError(t, err)
	require.Len(t, report.Corrupt, 1)
}

func TestScrub_Unverifiable(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "b"))

	// A pre-checksum object: file present, no sidecar.
	require.NoError(t, os.WriteFile(filepath.Join(root, "b", "legacy.txt"), []byte("no sidecar"), 0o600))

	report, err := s.Scrub(ctx, ScrubOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Scanned)
	require.Equal(t, 1, report.Unverifiable)
	require.True(t, report.Healthy())
}

func TestVerifyReads(t *testing.T) {
	root := t.TempDir()
	s, err := New(root, WithVerifyReads(true))
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "b"))
	putContent(t, s, "b", "ok.txt", []byte("healthy"))

	// A healthy object reads fine.
	obj, err := s.GetObject(ctx, "b", "ok.txt")
	require.NoError(t, err)
	require.NoError(t, obj.Reader.Close())

	// Corrupt it: verify-on-read refuses to serve it.
	putContent(t, s, "b", "bad.txt", []byte("will be corrupted"))
	corrupt(t, root, "b", "bad.txt")

	_, err = s.GetObject(ctx, "b", "bad.txt")
	require.ErrorIs(t, err, fs.ErrIntegrity)
}
