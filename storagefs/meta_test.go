package storagefs

import (
	"bytes"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// TestSidecarlessFileReadable guards backward compatibility: files placed in a
// pre-sidecar data directory stay readable with default metadata and a
// recomputed ETag.
func TestSidecarlessFileReadable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "legacy"))

	// Simulate a pre-sidecar object by writing the file directly.
	content := []byte("legacy content")
	require.NoError(t, os.WriteFile(filepath.Join(root, "legacy", "old.txt"), content, 0o600))

	obj, err := s.GetObject(ctx, "legacy", "old.txt")
	require.NoError(t, err)

	defer func() { _ = obj.Reader.Close() }()

	data, err := io.ReadAll(obj.Reader)
	require.NoError(t, err)
	require.Equal(t, content, data)
	require.Equal(t, fmt.Sprintf("%x", md5.Sum(content)), obj.ETag) //nolint:gosec // MD5 is required for S3 ETag compatibility.
	require.True(t, obj.Metadata.IsZero())

	// Tagging works on legacy files too (creates the sidecar on demand).
	require.NoError(t, s.PutObjectTagging(ctx, "legacy", "old.txt", []fs.Tag{{Key: "k", Value: "v"}}))

	tags, err := s.GetObjectTagging(ctx, "legacy", "old.txt")
	require.NoError(t, err)
	require.Equal(t, []fs.Tag{{Key: "k", Value: "v"}}, tags)
}

// TestCorruptSidecarTolerated guards that a damaged sidecar degrades to
// sidecar-less behavior instead of making the object unreadable.
func TestCorruptSidecarTolerated(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	content := []byte("content")
	_, err = s.PutObject(ctx, &fs.PutObjectRequest{
		Bucket:   "bucket-a",
		Key:      "obj.txt",
		Reader:   bytes.NewReader(content),
		Size:     int64(len(content)),
		Metadata: fs.ObjectMetadata{ContentType: "text/plain"},
	})
	require.NoError(t, err)

	// Corrupt the sidecar on disk.
	sum := sha256.Sum256([]byte("obj.txt"))
	sidecarPath := filepath.Join(root, metaDir, "bucket-a", hex.EncodeToString(sum[:])+".json")
	require.NoError(t, os.WriteFile(sidecarPath, []byte("{not json"), 0o600))

	obj, err := s.GetObject(ctx, "bucket-a", "obj.txt")
	require.NoError(t, err)

	defer func() { _ = obj.Reader.Close() }()

	data, err := io.ReadAll(obj.Reader)
	require.NoError(t, err)
	require.Equal(t, content, data)
	// Metadata is lost but the ETag falls back to recompute.
	require.Equal(t, fmt.Sprintf("%x", md5.Sum(content)), obj.ETag) //nolint:gosec // MD5 is required for S3 ETag compatibility.
}

// TestMetaDirHiddenFromBuckets guards that the sidecar tree never shows up as
// a bucket.
func TestMetaDirHiddenFromBuckets(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, err := New(root)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, s.CreateBucket(ctx, "bucket-a"))

	_, err = s.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: "bucket-a",
		Key:    "obj.txt",
		Reader: bytes.NewReader([]byte("x")),
		Size:   1,
	})
	require.NoError(t, err)

	// Start a multipart upload so .multipart exists too.
	_, err = s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "bucket-a", Key: "big.bin"})
	require.NoError(t, err)

	buckets, err := s.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	require.Equal(t, "bucket-a", buckets[0].Name)

	// Deleting the bucket removes its sidecar tree as well.
	require.NoError(t, s.DeleteObject(ctx, "bucket-a", "obj.txt"))
	require.NoError(t, s.DeleteBucket(ctx, "bucket-a"))

	_, err = os.Stat(filepath.Join(root, metaDir, "bucket-a"))
	require.True(t, os.IsNotExist(err))
}
