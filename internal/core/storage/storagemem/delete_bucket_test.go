package storagemem_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestStorage_DeleteBucket(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucketName = "bucket"

	// Create bucket
	err := storage.CreateBucket(ctx, bucketName)
	require.NoError(t, err)

	// Delete bucket
	err = storage.DeleteBucket(ctx, bucketName)
	require.NoError(t, err)

	// Verify it's deleted
	buckets, err := storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Empty(t, buckets)
}

func TestStorage_DeleteBucket_NotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	err := storage.DeleteBucket(ctx, "nonexistent")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func TestStorage_DeleteBucket_NotEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucketName = "bucket"

	// Create bucket and add an object
	err := storage.CreateBucket(ctx, bucketName)
	require.NoError(t, err)

	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucketName,
		Key:    "test.txt",
		Reader: bytes.NewReader([]byte{}),
		Size:   0,
	})
	require.NoError(t, err)

	// Try to delete non-empty bucket
	err = storage.DeleteBucket(ctx, bucketName)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bucket not empty")
}
