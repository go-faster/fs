package storagefs_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagefs"
)

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
