package storagefs_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/storage/storagefs"
)

// TestPutObject_WindowsPathSeparators verifies that S3 keys with forward slashes
// are correctly converted to OS-native path separators.
// On Windows, this test would FAIL if toOSPath is not implemented correctly.
func TestPutObject_WindowsPathSeparators(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Create bucket.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// S3 key with forward slashes (S3 standard).
	const s3Key = "path/to/nested/file.txt"

	content := []byte("test content")

	// Put object using S3-style key.
	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader(content),
		Bucket: "test-bucket",
		Key:    s3Key,
		Size:   int64(len(content)),
	})
	require.NoError(t, err)

	// The file MUST be stored with OS-native separators.
	// On Windows: root\test-bucket\path\to\nested\file.txt
	// On Unix: root/test-bucket/path/to/nested/file.txt
	expectedPath := filepath.Join(root, "test-bucket", filepath.FromSlash(s3Key))

	// Verify file exists at the correct OS-native path.
	info, err := os.Stat(expectedPath)
	require.NoError(t, err, "File should exist at OS-native path: %s", expectedPath)
	require.False(t, info.IsDir())
	require.Equal(t, int64(len(content)), info.Size())

	// CRITICAL: Verify that proper directory structure was created.
	// If toOSPath is broken, no intermediate directories would exist.
	if runtime.GOOS == "windows" {
		// Verify each intermediate directory exists.
		pathDir := filepath.Join(root, "test-bucket", "path")
		info, err := os.Stat(pathDir)
		require.NoError(t, err, "Intermediate directory 'path' should exist")
		require.True(t, info.IsDir())

		toDir := filepath.Join(root, "test-bucket", "path", "to")
		info, err = os.Stat(toDir)
		require.NoError(t, err, "Intermediate directory 'to' should exist")
		require.True(t, info.IsDir())

		nestedDir := filepath.Join(root, "test-bucket", "path", "to", "nested")
		info, err = os.Stat(nestedDir)
		require.NoError(t, err, "Intermediate directory 'nested' should exist")
		require.True(t, info.IsDir())

		// Verify the filename itself doesn't contain forward slashes.
		actualFilename := filepath.Base(expectedPath)
		require.False(t, strings.Contains(actualFilename, "/"),
			"Filename should not contain forward slashes: %s", actualFilename)
		require.Equal(t, "file.txt", actualFilename)
	}

	// Verify intermediate directories were created with proper structure.
	// On Windows, we should have: path\to\nested\file.txt
	// NOT: "path/to/nested/file.txt" as a single filename
	dirPath := filepath.Dir(expectedPath)
	dirInfo, err := os.Stat(dirPath)
	require.NoError(t, err, "Intermediate directory should exist: %s", dirPath)
	require.True(t, dirInfo.IsDir())

	// Count the directory depth to ensure structure is correct.
	relPath, err := filepath.Rel(filepath.Join(root, "test-bucket"), expectedPath)
	require.NoError(t, err)

	depth := strings.Count(relPath, string(filepath.Separator))
	require.Equal(t, 3, depth, "Should have 3 levels: path/to/nested/file.txt")
}

// TestGetObject_WindowsPathSeparators verifies that GetObject can retrieve files
// using S3-style keys with forward slashes.
// On Windows, this test would FAIL if toOSPath is not implemented correctly.
func TestGetObject_WindowsPathSeparators(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Create bucket.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Manually create a file with OS-native path separators.
	const s3Key = "dir1/dir2/file.txt"

	content := []byte("test data")

	osPath := filepath.Join(root, "test-bucket", filepath.FromSlash(s3Key))
	err = os.MkdirAll(filepath.Dir(osPath), 0750)
	require.NoError(t, err)

	err = os.WriteFile(osPath, content, 0600)
	require.NoError(t, err)

	// Try to get object using S3-style key with forward slashes.
	resp, err := storage.GetObject(ctx, "test-bucket", s3Key)
	require.NoError(t, err, "GetObject should work with S3-style keys")
	require.NotNil(t, resp)

	defer func() { _ = resp.Reader.Close() }()

	// Verify content.
	buf := make([]byte, len(content))
	n, err := resp.Reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, len(content), n)
	require.Equal(t, content, buf)
}

// TestDeleteObject_WindowsPathSeparators verifies that DeleteObject works with
// S3-style keys with forward slashes.
// On Windows, this test would FAIL if toOSPath is not implemented correctly.
func TestDeleteObject_WindowsPathSeparators(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Create bucket.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Create a file with OS-native path separators.
	const s3Key = "nested/path/file.txt"

	content := []byte("content")

	osPath := filepath.Join(root, "test-bucket", filepath.FromSlash(s3Key))
	err = os.MkdirAll(filepath.Dir(osPath), 0750)
	require.NoError(t, err)

	err = os.WriteFile(osPath, content, 0600)
	require.NoError(t, err)

	// Verify file exists.
	_, err = os.Stat(osPath)
	require.NoError(t, err)

	// Delete using S3-style key.
	err = storage.DeleteObject(ctx, "test-bucket", s3Key)
	require.NoError(t, err)

	// Verify file is deleted.
	_, err = os.Stat(osPath)
	require.True(t, os.IsNotExist(err), "File should be deleted")
}

// TestPutObject_DeepNestedPath_WindowsFailure tests a deeply nested path
// that would definitely fail on Windows if path conversion is broken.
func TestPutObject_DeepNestedPath_WindowsFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Very deep S3-style path.
	const s3Key = "a/b/c/d/e/f/g/h/i/j/file.txt"

	content := []byte("deep content")

	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader(content),
		Bucket: "test-bucket",
		Key:    s3Key,
		Size:   int64(len(content)),
	})
	require.NoError(t, err)

	// Verify correct path structure.
	expectedPath := filepath.Join(root, "test-bucket", filepath.FromSlash(s3Key))
	info, err := os.Stat(expectedPath)
	require.NoError(t, err)
	require.False(t, info.IsDir())

	// Count directory depth - should have 10 subdirectories (a through j).
	if runtime.GOOS == "windows" {
		// On Windows, verify the path contains backslashes, not forward slashes.
		require.True(t, strings.Contains(expectedPath, "\\"),
			"Windows path should contain backslashes")
		require.False(t, strings.Contains(filepath.Base(expectedPath), "/"),
			"Filename should not contain forward slashes")
	}

	// Verify we can read it back.
	resp, err := storage.GetObject(ctx, "test-bucket", s3Key)
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	buf := make([]byte, len(content))
	n, err := resp.Reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, len(content), n)
	require.Equal(t, content, buf)
}

// TestPutGetDelete_MultipleSeparators tests keys with multiple consecutive slashes.
// This would fail on Windows if path conversion is incorrect.
func TestPutGetDelete_MultipleSeparators(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// S3 allows multiple slashes (though uncommon).
	const s3Key = "path//with///multiple////slashes/file.txt"

	content := []byte("multi-slash content")

	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader(content),
		Bucket: "test-bucket",
		Key:    s3Key,
		Size:   int64(len(content)),
	})
	require.NoError(t, err)

	// Verify file exists at correct path.
	expectedPath := filepath.Join(root, "test-bucket", filepath.FromSlash(s3Key))
	_, err = os.Stat(expectedPath)
	require.NoError(t, err)

	// Get object.
	resp, err := storage.GetObject(ctx, "test-bucket", s3Key)
	require.NoError(t, err)

	buf := make([]byte, len(content))
	n, err := resp.Reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, len(content), n)
	require.Equal(t, content, buf)

	// Close file handle before deletion (Windows requirement).
	err = resp.Reader.Close()
	require.NoError(t, err)

	// Delete object.
	err = storage.DeleteObject(ctx, "test-bucket", s3Key)
	require.NoError(t, err)

	_, err = os.Stat(expectedPath)
	require.True(t, os.IsNotExist(err))
}

// TestPutObject_TrailingSlash tests keys with trailing slashes.
// This is an edge case that could fail on Windows.
func TestPutObject_TrailingSlash(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Key with trailing slash (represents a directory-like object in S3).
	const s3Key = "path/to/dir/"

	content := []byte("dir marker")

	err = storage.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader(content),
		Bucket: "test-bucket",
		Key:    s3Key,
		Size:   int64(len(content)),
	})
	require.NoError(t, err)

	// Verify file exists (with trailing separator converted).
	expectedPath := filepath.Join(root, "test-bucket", filepath.FromSlash(s3Key))
	_, err = os.Stat(expectedPath)
	require.NoError(t, err)
}

// TestRoundTrip_ComplexPaths verifies complete round-trip with various complex paths.
// This comprehensive test would catch Windows path issues.
func TestRoundTrip_ComplexPaths(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	testCases := []struct {
		name string
		key  string
	}{
		{"single level", "dir/file.txt"},
		{"double level", "dir1/dir2/file.txt"},
		{"triple level", "a/b/c/file.txt"},
		{"with dots", "path/to/file.with.dots.txt"},
		{"with dashes", "path/to/file-with-dashes.txt"},
		{"with underscores", "path_to/file_name.txt"},
		{"mixed separators depth", "a/b/c/d/e/f/file.txt"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			content := []byte("content for " + tc.name)

			// Put.
			err := storage.PutObject(ctx, &fs.PutObjectRequest{
				Reader: bytes.NewReader(content),
				Bucket: "test-bucket",
				Key:    tc.key,
				Size:   int64(len(content)),
			})
			require.NoError(t, err)

			// Verify OS-native path exists.
			expectedPath := filepath.Join(root, "test-bucket", filepath.FromSlash(tc.key))
			info, err := os.Stat(expectedPath)
			require.NoError(t, err)
			require.False(t, info.IsDir())

			// Get.
			resp, err := storage.GetObject(ctx, "test-bucket", tc.key)
			require.NoError(t, err)

			buf := make([]byte, len(content))
			n, err := resp.Reader.Read(buf)
			_ = resp.Reader.Close()

			require.NoError(t, err)
			require.Equal(t, len(content), n)
			require.Equal(t, content, buf)

			// Delete.
			err = storage.DeleteObject(ctx, "test-bucket", tc.key)
			require.NoError(t, err)

			_, err = os.Stat(expectedPath)
			require.True(t, os.IsNotExist(err))
		})
	}
}
