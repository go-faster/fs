package storagefs_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorage_DeleteBucket(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	const bucket = "test-bucket"

	// Create bucket first
	err := storage.CreateBucket(ctx, bucket)
	require.NoError(t, err)

	// Delete bucket
	err = storage.DeleteBucket(ctx, bucket)
	require.NoError(t, err)

	// Verify bucket is deleted by listing
	buckets, err := storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Empty(t, buckets)
}

func TestStorage_DeleteBucket_NotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	// Try to delete non-existent bucket
	err := storage.DeleteBucket(ctx, "nonexistent")
	require.Error(t, err)
}
