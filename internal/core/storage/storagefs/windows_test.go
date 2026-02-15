// nolint // False positives on valid whitespace from wsl linter.
package storagefs_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/storage/storagefs"
)

func TestStorage_WindowsPathCompatibility(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Create a bucket.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	tests := []struct {
		name    string
		key     string
		content []byte
	}{
		{
			name:    "simple file",
			key:     "file.txt",
			content: []byte("simple content"),
		},
		{
			name:    "nested path with forward slashes",
			key:     "path/to/file.txt",
			content: []byte("nested content"),
		},
		{
			name:    "deep nested path",
			key:     "a/b/c/d/e/file.txt",
			content: []byte("deep content"),
		},
		{
			name:    "path with dots",
			key:     "path/to/file.with.dots.txt",
			content: []byte("dots content"),
		},
		{
			name:    "path with special chars",
			key:     "path/to/file-with_special.txt",
			content: []byte("special content"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Put object.
			err := storage.PutObject(ctx, &fs.PutObjectRequest{
				Reader: bytes.NewReader(tt.content),
				Bucket: "test-bucket",
				Key:    tt.key,
				Size:   int64(len(tt.content)),
			})
			require.NoError(t, err)

			// Verify object exists on disk with correct OS path.
			expectedPath := filepath.Join(root, "test-bucket", filepath.FromSlash(tt.key))
			info, err := os.Stat(expectedPath)
			require.NoError(t, err)
			require.False(t, info.IsDir())
			require.Equal(t, int64(len(tt.content)), info.Size())

			// Get object.
			resp, err := storage.GetObject(ctx, "test-bucket", tt.key)
			require.NoError(t, err)
			require.NotNil(t, resp)

			defer func() { _ = resp.Reader.Close() }()

			// Verify content.
			buf := make([]byte, len(tt.content))
			n, err := resp.Reader.Read(buf)
			require.NoError(t, err)
			require.Equal(t, len(tt.content), n)
			require.Equal(t, tt.content, buf)

			// Delete object.
			err = storage.DeleteObject(ctx, "test-bucket", tt.key)
			require.NoError(t, err)

			// Verify object is deleted.
			_, err = os.Stat(expectedPath)
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestStorage_ListObjects_WindowsPathCompatibility(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Create a bucket.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Create objects with nested paths.
	objects := []string{
		"file1.txt",
		"dir1/file2.txt",
		"dir1/subdir/file3.txt",
		"dir2/file4.txt",
	}

	for _, key := range objects {
		err := storage.PutObject(ctx, &fs.PutObjectRequest{
			Reader: bytes.NewReader([]byte("content")),
			Bucket: "test-bucket",
			Key:    key,
			Size:   7,
		})
		require.NoError(t, err)
	}

	// List all objects - keys should always use forward slashes (S3 convention).
	result, err := storage.ListObjects(ctx, "test-bucket", "")
	require.NoError(t, err)
	require.Len(t, result, len(objects))

	// Verify all keys use forward slashes, not backslashes.
	keys := make([]string, len(result))
	for i, obj := range result {
		keys[i] = obj.Key
		require.NotContains(t, obj.Key, "\\", "Key should not contain backslashes")
	}

	require.ElementsMatch(t, objects, keys)
}

func TestStorage_MultipartUpload_WindowsPathCompatibility(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Create a bucket.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Create multipart upload with nested key.
	const nestedKey = "path/to/multipart/file.txt"

	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", nestedKey)
	require.NoError(t, err)
	require.NotEmpty(t, upload.UploadID)

	// Upload parts.
	part1Data := []byte("part 1 content")
	part1, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        nestedKey,
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(part1Data),
	})
	require.NoError(t, err)

	part2Data := []byte("part 2 content")
	part2, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        nestedKey,
		UploadID:   upload.UploadID,
		PartNumber: 2,
		Reader:     bytes.NewReader(part2Data),
	})
	require.NoError(t, err)

	// Complete multipart upload.
	resp, err := storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      nestedKey,
		UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{
			{PartNumber: 1, ETag: part1.ETag},
			{PartNumber: 2, ETag: part2.ETag},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.ETag)

	// Verify object exists with correct OS path.
	expectedPath := filepath.Join(root, "test-bucket", filepath.FromSlash(nestedKey))
	info, err := os.Stat(expectedPath)
	require.NoError(t, err)
	require.False(t, info.IsDir())
	require.Equal(t, int64(len(part1Data)+len(part2Data)), info.Size())

	// Get object and verify content.
	obj, err := storage.GetObject(ctx, "test-bucket", nestedKey)
	require.NoError(t, err)

	defer func() { _ = obj.Reader.Close() }()

	expectedContent := append(part1Data, part2Data...)
	buf := make([]byte, len(expectedContent))
	n, err := obj.Reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, len(expectedContent), n)
	require.Equal(t, expectedContent, buf)
}
