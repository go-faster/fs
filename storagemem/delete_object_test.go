package storagemem_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestStorage_DeleteObject(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const (
		bucket = "test-bucket"
		key    = "test.txt"
	)

	// Create bucket and put object
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucket,
		Key:    key,
		Reader: bytes.NewReader([]byte("content")),
		Size:   7,
	})
	require.NoError(t, err)

	// Delete object
	err = storage.DeleteObject(ctx, bucket, key)
	require.NoError(t, err)

	// Verify it's deleted
	objects, err := storage.ListObjects(ctx, bucket, "")
	require.NoError(t, err)
	require.Empty(t, objects)
}

func TestStorage_DeleteObject_BucketNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	err := storage.DeleteObject(ctx, "nonexistent", "test.txt")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func TestStorage_DeleteObject_ObjectNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucket = "test-bucket"

	// Create bucket
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Try to delete nonexistent object
	err = storage.DeleteObject(ctx, bucket, "nonexistent.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}
