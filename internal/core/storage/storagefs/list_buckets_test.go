package storagefs_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorage_ListBuckets(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := newStorage(t)

	for _, name := range []string{"bucket1", "bucket2", "bucket3"} {
		err := storage.CreateBucket(ctx, name)
		require.NoError(t, err)
	}

	buckets, err := storage.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 3)

	expectedNames := map[string]bool{"bucket1": true, "bucket2": true, "bucket3": true}
	for _, bucket := range buckets {
		require.Contains(t, expectedNames, bucket.Name)
		delete(expectedNames, bucket.Name)
	}

	require.Empty(t, expectedNames, "Not all expected buckets were found")
}
