package storagemem_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorage_CreateBucket(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucketName = "bucket"

	err := storage.CreateBucket(ctx, "bucket")
	require.NoError(t, err)

	buckets, err := storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	require.Equal(t, bucketName, buckets[0].Name)
}

func TestStorage_CreateBucket_AlreadyExists(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucketName = "bucket"

	err := storage.CreateBucket(ctx, bucketName)
	require.NoError(t, err)

	err = storage.CreateBucket(ctx, bucketName)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bucket already exists")
}
