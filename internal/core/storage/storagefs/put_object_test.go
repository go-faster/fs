package storagefs_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestStorage_PutObject(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const (
		bucket = "test-bucket"
		key    = "test-object.txt"
	)
	content := []byte("hello, world!")

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

	// Verify object exists by listing
	objects, err := storage.ListObjects(ctx, bucket, "")
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, key, objects[0].Key)
	require.Equal(t, int64(len(content)), objects[0].Size)
}

func TestStorage_PutObject_NestedKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const (
		bucket = "test-bucket"
		key    = "path/to/nested/object.txt"
	)
	content := []byte("nested content")

	// Create bucket first
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Put object with nested key
	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucket,
		Key:    key,
		Reader: bytes.NewReader(content),
		Size:   int64(len(content)),
	})
	require.NoError(t, err)

	// Verify object exists by listing with prefix
	objects, err := storage.ListObjects(ctx, bucket, "path/to/")
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, key, objects[0].Key)
}

func TestStorage_PutObject_Overwrite(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const (
		bucket = "test-bucket"
		key    = "test-object.txt"
	)
	content1 := []byte("original content")
	content2 := []byte("updated content with more data")

	// Create bucket first
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Put object first time
	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucket,
		Key:    key,
		Reader: bytes.NewReader(content1),
		Size:   int64(len(content1)),
	})
	require.NoError(t, err)

	// Overwrite object
	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucket,
		Key:    key,
		Reader: bytes.NewReader(content2),
		Size:   int64(len(content2)),
	})
	require.NoError(t, err)

	// Verify object has new size
	objects, err := storage.ListObjects(ctx, bucket, "")
	require.NoError(t, err)
	require.Len(t, objects, 1)
	require.Equal(t, int64(len(content2)), objects[0].Size)
}
