package storagefs_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/storage/storagefs"
)

func TestStorage_BucketExists(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Create a bucket.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Verify bucket exists.
	exists, err := storage.BucketExists(ctx, "test-bucket")
	require.NoError(t, err)
	require.True(t, exists)
}

func TestStorage_BucketExists_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Check nonexistent bucket.
	exists, err := storage.BucketExists(ctx, "nonexistent-bucket")
	require.NoError(t, err)
	require.False(t, exists)
}

func TestStorage_BucketExists_File(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Create a file instead of a directory.
	filePath := filepath.Join(root, "not-a-bucket")
	err = os.WriteFile(filePath, []byte("content"), 0600)
	require.NoError(t, err)

	// Should return error since it's not a directory.
	exists, err := storage.BucketExists(ctx, "not-a-bucket")
	require.Error(t, err)
	require.False(t, exists)
}
