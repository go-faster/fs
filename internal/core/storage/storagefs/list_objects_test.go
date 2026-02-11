package storagefs_test

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

	const (
		bucket  = "bucket"
		object1 = "object1"
	)
	data := []byte("hello, world!\n")

	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: bucket,
		Key:    object1,
		Reader: bytes.NewReader(data),
	})
	require.NoError(t, err)

	t.Run("WithPrefix", func(t *testing.T) {
		t.Run("Positive", func(t *testing.T) {
			objects, err := storage.ListObjects(ctx, bucket, object1[:3])
			require.NoError(t, err)
			require.Len(t, objects, 1)
			require.Equal(t, object1, objects[0].Key)
		})
		t.Run("Negative", func(t *testing.T) {
			objects, err := storage.ListObjects(ctx, bucket, "nonexistent")
			require.NoError(t, err)
			require.Empty(t, objects)
		})
	})
	t.Run("WithoutPrefix", func(t *testing.T) {
		objects, err := storage.ListObjects(ctx, bucket, "")
		require.NoError(t, err)
		require.Len(t, objects, 1)
		require.Equal(t, object1, objects[0].Key)
	})
}
