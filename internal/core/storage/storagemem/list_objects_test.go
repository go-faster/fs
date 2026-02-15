package storagemem_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestStorage_ListObjects(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucket = "test-bucket"

	// Create bucket
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Initially empty
	objects, err := storage.ListObjects(ctx, bucket, "")
	require.NoError(t, err)
	require.Empty(t, objects)

	// Add some objects
	for _, key := range []string{"file1.txt", "file2.txt", "dir/file3.txt"} {
		err = storage.PutObject(ctx, &fs.PutObjectRequest{
			Bucket: bucket,
			Key:    key,
			Reader: bytes.NewReader([]byte("content")),
			Size:   7,
		})
		require.NoError(t, err)
	}

	// List all objects
	objects, err = storage.ListObjects(ctx, bucket, "")
	require.NoError(t, err)
	require.Len(t, objects, 3)
}

func TestStorage_ListObjects_WithPrefix(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucket = "test-bucket"

	// Create bucket
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Add objects with different prefixes
	objects := []string{
		"docs/readme.txt",
		"docs/guide.txt",
		"images/logo.png",
		"images/banner.jpg",
		"index.html",
	}

	for _, key := range objects {
		err = storage.PutObject(ctx, &fs.PutObjectRequest{
			Bucket: bucket,
			Key:    key,
			Reader: bytes.NewReader([]byte("content")),
			Size:   7,
		})
		require.NoError(t, err)
	}

	// List objects with "docs/" prefix
	result, err := storage.ListObjects(ctx, bucket, "docs/")
	require.NoError(t, err)
	require.Len(t, result, 2)

	// List objects with "images/" prefix
	result, err = storage.ListObjects(ctx, bucket, "images/")
	require.NoError(t, err)
	require.Len(t, result, 2)

	// List objects with no matching prefix
	result, err = storage.ListObjects(ctx, bucket, "videos/")
	require.NoError(t, err)
	require.Empty(t, result)
}

func TestStorage_ListObjects_BucketNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	_, err := storage.ListObjects(ctx, "nonexistent", "")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}
