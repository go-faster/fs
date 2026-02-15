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

func TestStorage_CreateMultipartUpload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Create bucket first.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Create multipart upload.
	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "test-key")
	require.NoError(t, err)
	require.NotEmpty(t, upload.UploadID)
	require.Equal(t, "test-bucket", upload.Bucket)
	require.Equal(t, "test-key", upload.Key)
	require.False(t, upload.Initiated.IsZero())
}

func TestStorage_CreateMultipartUpload_BucketNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Try to create multipart upload for nonexistent bucket.
	_, err = storage.CreateMultipartUpload(ctx, "nonexistent-bucket", "test-key")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func TestStorage_UploadPart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "test-key")
	require.NoError(t, err)

	// Upload part.
	partData := []byte("hello world part 1")
	part, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key",
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(partData),
		Size:       int64(len(partData)),
	})
	require.NoError(t, err)
	require.Equal(t, 1, part.PartNumber)
	require.NotEmpty(t, part.ETag)
	require.Equal(t, int64(len(partData)), part.Size)
}

func TestStorage_UploadPart_UploadNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Try to upload part for nonexistent upload.
	_, err = storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key",
		UploadID:   "nonexistent-upload-id",
		PartNumber: 1,
		Reader:     bytes.NewReader([]byte("data")),
		Size:       4,
	})
	require.Error(t, err)
}

func TestStorage_UploadPart_MultipleParts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "test-key")
	require.NoError(t, err)

	// Upload multiple parts.
	parts := [][]byte{
		[]byte("part one data"),
		[]byte("part two data here"),
		[]byte("part three"),
	}

	var uploadedParts []fs.CompletedPart
	for i, partData := range parts {
		part, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket:     "test-bucket",
			Key:        "test-key",
			UploadID:   upload.UploadID,
			PartNumber: i + 1,
			Reader:     bytes.NewReader(partData),
			Size:       int64(len(partData)),
		})
		require.NoError(t, err)
		require.Equal(t, i+1, part.PartNumber)
		uploadedParts = append(uploadedParts, fs.CompletedPart{
			PartNumber: part.PartNumber,
			ETag:       part.ETag,
		})
	}

	require.Len(t, uploadedParts, 3)
}

func TestStorage_CompleteMultipartUpload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "test-key.txt")
	require.NoError(t, err)

	// Upload parts.
	part1Data := []byte("Hello, ")
	part2Data := []byte("World!")

	part1, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key.txt",
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(part1Data),
		Size:       int64(len(part1Data)),
	})
	require.NoError(t, err)

	part2, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key.txt",
		UploadID:   upload.UploadID,
		PartNumber: 2,
		Reader:     bytes.NewReader(part2Data),
		Size:       int64(len(part2Data)),
	})
	require.NoError(t, err)

	// Complete multipart upload.
	resp, err := storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      "test-key.txt",
		UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{
			{PartNumber: 1, ETag: part1.ETag},
			{PartNumber: 2, ETag: part2.ETag},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "test-bucket", resp.Bucket)
	require.Equal(t, "test-key.txt", resp.Key)
	require.NotEmpty(t, resp.ETag)

	// Verify object was created with correct content.
	obj, err := storage.GetObject(ctx, "test-bucket", "test-key.txt")
	require.NoError(t, err)
	defer obj.Reader.Close()

	data, err := os.ReadFile(filepath.Join(t.TempDir()))
	_ = data

	buf := make([]byte, 100)
	n, err := obj.Reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "Hello, World!", string(buf[:n]))
}

func TestStorage_CompleteMultipartUpload_OutOfOrderParts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "test-key.txt")
	require.NoError(t, err)

	// Upload parts out of order.
	part3Data := []byte("THREE")
	part1Data := []byte("ONE")
	part2Data := []byte("TWO")

	// Upload part 3 first.
	part3, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key.txt",
		UploadID:   upload.UploadID,
		PartNumber: 3,
		Reader:     bytes.NewReader(part3Data),
		Size:       int64(len(part3Data)),
	})
	require.NoError(t, err)

	// Then part 1.
	part1, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key.txt",
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(part1Data),
		Size:       int64(len(part1Data)),
	})
	require.NoError(t, err)

	// Finally part 2.
	part2, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key.txt",
		UploadID:   upload.UploadID,
		PartNumber: 2,
		Reader:     bytes.NewReader(part2Data),
		Size:       int64(len(part2Data)),
	})
	require.NoError(t, err)

	// Complete with parts in wrong order - should still work.
	resp, err := storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      "test-key.txt",
		UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{
			{PartNumber: 3, ETag: part3.ETag},
			{PartNumber: 1, ETag: part1.ETag},
			{PartNumber: 2, ETag: part2.ETag},
		},
	})
	require.NoError(t, err)

	// Verify content is in correct order.
	obj, err := storage.GetObject(ctx, "test-bucket", "test-key.txt")
	require.NoError(t, err)
	defer obj.Reader.Close()

	buf := make([]byte, 100)
	n, err := obj.Reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "ONETWOTHREE", string(buf[:n]))
	require.Equal(t, "test-bucket", resp.Bucket)
}

func TestStorage_CompleteMultipartUpload_UploadNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Try to complete nonexistent upload.
	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      "test-key",
		UploadID: "nonexistent-upload-id",
		Parts:    []fs.CompletedPart{},
	})
	require.Error(t, err)
}

func TestStorage_AbortMultipartUpload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "test-key")
	require.NoError(t, err)

	// Upload a part.
	partData := []byte("some data")
	_, err = storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key",
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(partData),
		Size:       int64(len(partData)),
	})
	require.NoError(t, err)

	// Verify upload directory exists.
	uploadDir := filepath.Join(root, ".multipart", upload.UploadID)
	_, err = os.Stat(uploadDir)
	require.NoError(t, err)

	// Abort upload.
	err = storage.AbortMultipartUpload(ctx, "test-bucket", "test-key", upload.UploadID)
	require.NoError(t, err)

	// Verify upload directory was removed.
	_, err = os.Stat(uploadDir)
	require.True(t, os.IsNotExist(err))
}

func TestStorage_AbortMultipartUpload_UploadNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Try to abort nonexistent upload.
	err = storage.AbortMultipartUpload(ctx, "test-bucket", "test-key", "nonexistent-upload-id")
	require.Error(t, err)
}

func TestStorage_MultipartUpload_MetadataPersistence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	storage, err := storagefs.New(root)
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Create multipart upload.
	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "test-key")
	require.NoError(t, err)

	// Verify metadata file was created.
	metadataPath := filepath.Join(root, ".multipart", upload.UploadID, "metadata.json")
	_, err = os.Stat(metadataPath)
	require.NoError(t, err)

	// Read metadata file and verify contents.
	data, err := os.ReadFile(metadataPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "test-bucket")
	require.Contains(t, string(data), "test-key")
	require.Contains(t, string(data), upload.UploadID)
}

func TestStorage_MultipartUpload_NestedKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Create multipart upload with nested key.
	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "path/to/nested/file.txt")
	require.NoError(t, err)

	// Upload part.
	partData := []byte("nested content")
	part, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "path/to/nested/file.txt",
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(partData),
		Size:       int64(len(partData)),
	})
	require.NoError(t, err)

	// Complete.
	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      "path/to/nested/file.txt",
		UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{
			{PartNumber: 1, ETag: part.ETag},
		},
	})
	require.NoError(t, err)

	// Verify object exists at nested path.
	obj, err := storage.GetObject(ctx, "test-bucket", "path/to/nested/file.txt")
	require.NoError(t, err)
	defer obj.Reader.Close()
	require.Equal(t, int64(len(partData)), obj.Size)
}

func TestStorage_MultipartUpload_LargeFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Create upload.
	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "large-file.bin")
	require.NoError(t, err)

	// Create 3 parts of 1MB each.
	partSize := 1024 * 1024
	part1Data := make([]byte, partSize)
	part2Data := make([]byte, partSize)
	part3Data := make([]byte, partSize)

	for i := range part1Data {
		part1Data[i] = byte(i % 256)
	}
	for i := range part2Data {
		part2Data[i] = byte((i + 100) % 256)
	}
	for i := range part3Data {
		part3Data[i] = byte((i + 200) % 256)
	}

	// Upload parts.
	part1, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "large-file.bin",
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(part1Data),
		Size:       int64(len(part1Data)),
	})
	require.NoError(t, err)

	part2, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "large-file.bin",
		UploadID:   upload.UploadID,
		PartNumber: 2,
		Reader:     bytes.NewReader(part2Data),
		Size:       int64(len(part2Data)),
	})
	require.NoError(t, err)

	part3, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "large-file.bin",
		UploadID:   upload.UploadID,
		PartNumber: 3,
		Reader:     bytes.NewReader(part3Data),
		Size:       int64(len(part3Data)),
	})
	require.NoError(t, err)

	// Complete upload.
	resp, err := storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      "large-file.bin",
		UploadID: upload.UploadID,
		Parts: []fs.CompletedPart{
			{PartNumber: 1, ETag: part1.ETag},
			{PartNumber: 2, ETag: part2.ETag},
			{PartNumber: 3, ETag: part3.ETag},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.ETag)

	// Verify object size.
	obj, err := storage.GetObject(ctx, "test-bucket", "large-file.bin")
	require.NoError(t, err)
	defer obj.Reader.Close()
	require.Equal(t, int64(partSize*3), obj.Size)
}

func TestStorage_MultipartUpload_ConcurrentUploads(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	// Create two concurrent multipart uploads.
	upload1, err := storage.CreateMultipartUpload(ctx, "test-bucket", "file1.txt")
	require.NoError(t, err)

	upload2, err := storage.CreateMultipartUpload(ctx, "test-bucket", "file2.txt")
	require.NoError(t, err)

	// Upload parts to both.
	part1Data := []byte("content for file 1")
	part2Data := []byte("content for file 2")

	part1, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "file1.txt",
		UploadID:   upload1.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(part1Data),
		Size:       int64(len(part1Data)),
	})
	require.NoError(t, err)

	part2, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "file2.txt",
		UploadID:   upload2.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(part2Data),
		Size:       int64(len(part2Data)),
	})
	require.NoError(t, err)

	// Complete both uploads.
	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      "file1.txt",
		UploadID: upload1.UploadID,
		Parts:    []fs.CompletedPart{{PartNumber: 1, ETag: part1.ETag}},
	})
	require.NoError(t, err)

	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      "file2.txt",
		UploadID: upload2.UploadID,
		Parts:    []fs.CompletedPart{{PartNumber: 1, ETag: part2.ETag}},
	})
	require.NoError(t, err)

	// Verify both objects.
	obj1, err := storage.GetObject(ctx, "test-bucket", "file1.txt")
	require.NoError(t, err)
	defer obj1.Reader.Close()

	obj2, err := storage.GetObject(ctx, "test-bucket", "file2.txt")
	require.NoError(t, err)
	defer obj2.Reader.Close()

	require.Equal(t, int64(len(part1Data)), obj1.Size)
	require.Equal(t, int64(len(part2Data)), obj2.Size)
}

func TestStorage_MultipartUpload_OverwritePart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	// Setup.
	err = storage.CreateBucket(ctx, "test-bucket")
	require.NoError(t, err)

	upload, err := storage.CreateMultipartUpload(ctx, "test-bucket", "test-key.txt")
	require.NoError(t, err)

	// Upload part first time.
	part1DataOld := []byte("old data")
	_, err = storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key.txt",
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(part1DataOld),
		Size:       int64(len(part1DataOld)),
	})
	require.NoError(t, err)

	// Overwrite part.
	part1DataNew := []byte("new data content")
	partNew, err := storage.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     "test-bucket",
		Key:        "test-key.txt",
		UploadID:   upload.UploadID,
		PartNumber: 1,
		Reader:     bytes.NewReader(part1DataNew),
		Size:       int64(len(part1DataNew)),
	})
	require.NoError(t, err)

	// Complete with new part.
	_, err = storage.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket:   "test-bucket",
		Key:      "test-key.txt",
		UploadID: upload.UploadID,
		Parts:    []fs.CompletedPart{{PartNumber: 1, ETag: partNew.ETag}},
	})
	require.NoError(t, err)

	// Verify content is from new part.
	obj, err := storage.GetObject(ctx, "test-bucket", "test-key.txt")
	require.NoError(t, err)
	defer obj.Reader.Close()

	buf := make([]byte, 100)
	n, err := obj.Reader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, string(part1DataNew), string(buf[:n]))
}
