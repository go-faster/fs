package storagefs_test

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
		key    = "test-object.txt"
	)
	content := []byte("test content")

	// Create bucket first
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Put object
	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucket,
		Key:    key,
		Reader: bytes.NewReader(content),
		Size:   int64(len(content)),
	})
	require.NoError(t, err)

	// Delete object
	err = storage.DeleteObject(ctx, bucket, key)
	require.NoError(t, err)

	// Verify object is deleted by listing
	objects, err := storage.ListObjects(ctx, bucket, "")
	require.NoError(t, err)
	require.Empty(t, objects)
}

func TestStorage_DeleteObject_NotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucket = "test-bucket"

	// Create bucket first
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Try to delete non-existent object
	err = storage.DeleteObject(ctx, bucket, "nonexistent.txt")
	require.Error(t, err)
}
