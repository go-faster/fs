package storagemem_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorage_ListBuckets(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	// Initially empty
	buckets, err := storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Empty(t, buckets)

	// Create some buckets
	err = storage.CreateBucket(ctx, "bucket1")
	require.NoError(t, err)

	err = storage.CreateBucket(ctx, "bucket2")
	require.NoError(t, err)

	// List buckets
	buckets, err = storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 2)

	bucketNames := make(map[string]bool)
	for _, b := range buckets {
		bucketNames[b.Name] = true
	}
	require.True(t, bucketNames["bucket1"])
	require.True(t, bucketNames["bucket2"])
}
