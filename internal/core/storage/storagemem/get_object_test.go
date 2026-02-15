package storagemem_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestStorage_GetObject(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const (
		bucket = "test-bucket"
		key    = "test-object.txt"
	)

	content := []byte("hello, world!")

	// Create bucket and put object
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucket,
		Key:    key,
		Reader: bytes.NewReader(content),
		Size:   int64(len(content)),
	})
	require.NoError(t, err)

	// Get object
	resp, err := storage.GetObject(ctx, bucket, key)
	require.NoError(t, err)

	require.NotNil(t, resp)
	defer resp.Reader.Close()

	// Verify content
	data, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	require.Equal(t, content, data)
	require.Equal(t, int64(len(content)), resp.Size)
	require.NotEmpty(t, resp.ETag)
}

func TestStorage_GetObject_BucketNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	_, err := storage.GetObject(ctx, "nonexistent", "test.txt")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func TestStorage_GetObject_ObjectNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucket = "test-bucket"

	// Create bucket
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Try to get nonexistent object
	_, err = storage.GetObject(ctx, bucket, "nonexistent.txt")
	require.ErrorIs(t, err, fs.ErrObjectNotFound)
}
