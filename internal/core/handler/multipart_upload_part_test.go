package handler_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/go-faster/errors"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestHandler_UploadPart(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
		uploadID   = "test-upload-id"
		partNumber = 1
		partETag   = "abc123def456"
	)

	partData := []byte("test part data content")
	svc := baseMock()
	svc.UploadPartFunc = func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
		require.Equal(t, bucketName, req.Bucket)
		require.Equal(t, objectKey, req.Key)
		require.Equal(t, uploadID, req.UploadID)
		require.Equal(t, partNumber, req.PartNumber)
		// Read and verify the data.
		data, err := io.ReadAll(req.Reader)
		require.NoError(t, err)
		require.Equal(t, partData, data)

		return &fs.Part{
			PartNumber: partNumber,
			ETag:       partETag,
			Size:       int64(len(partData)),
		}, nil
	}
	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}
	info, err := core.PutObjectPart(ctx, bucketName, objectKey, uploadID, partNumber,
		bytes.NewReader(partData), int64(len(partData)), minio.PutObjectPartOptions{})
	require.NoError(t, err)
	require.Equal(t, partNumber, info.PartNumber)
	require.Equal(t, partETag, info.ETag)
}

func TestHandler_UploadPart_MultipleParts(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
		uploadID   = "test-upload-id"
	)

	parts := []struct {
		number int
		data   []byte
		etag   string
	}{
		{1, []byte("part one data"), "etag1"},
		{2, []byte("part two data here"), "etag2"},
		{3, []byte("part three"), "etag3"},
	}
	svc := baseMock()
	svc.UploadPartFunc = func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
		for _, p := range parts {
			if p.number == req.PartNumber {
				data, err := io.ReadAll(req.Reader)
				if err != nil {
					return nil, err
				}

				require.Equal(t, p.data, data)

				return &fs.Part{
					PartNumber: p.number,
					ETag:       p.etag,
					Size:       int64(len(p.data)),
				}, nil
			}
		}

		return nil, errors.New("unexpected part number")
	}
	ctx := t.Context()

	core := minio.Core{Client: newTestClient(t, svc)}
	for _, p := range parts {
		info, err := core.PutObjectPart(ctx, bucketName, objectKey, uploadID, p.number,
			bytes.NewReader(p.data), int64(len(p.data)), minio.PutObjectPartOptions{})
		require.NoError(t, err)
		require.Equal(t, p.number, info.PartNumber)
		require.Equal(t, p.etag, info.ETag)
	}
}

func TestHandler_UploadPart_UploadNotFound(t *testing.T) {
	t.Parallel()

	svc := baseMock()
	svc.UploadPartFunc = func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
		return nil, fs.ErrUploadNotFound
	}
	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}
	_, err := core.PutObjectPart(ctx, "bucket", "key", "invalid-upload-id", 1,
		bytes.NewReader([]byte("data")), 4, minio.PutObjectPartOptions{})
	require.Error(t, err)
}

func TestHandler_UploadPart_LargePart(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "large-object.bin"
		uploadID   = "large-upload-id"
		partNumber = 1
	)
	// Create 1MB part.
	partSize := 1024 * 1024

	partData := make([]byte, partSize)
	for i := range partData {
		partData[i] = byte(i % 256)
	}

	svc := baseMock()
	svc.UploadPartFunc = func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
		data, err := io.ReadAll(req.Reader)
		require.NoError(t, err)
		require.Len(t, data, partSize)
		require.Equal(t, partData, data)

		return &fs.Part{
			PartNumber: partNumber,
			ETag:       "large-etag",
			Size:       int64(len(data)),
		}, nil
	}
	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}
	info, err := core.PutObjectPart(ctx, bucketName, objectKey, uploadID, partNumber,
		bytes.NewReader(partData), int64(len(partData)), minio.PutObjectPartOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(partSize), info.Size)
}
func TestHandler_UploadPart_EmptyPart(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
		uploadID   = "test-upload-id"
	)

	svc := baseMock()
	svc.UploadPartFunc = func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
		data, err := io.ReadAll(req.Reader)
		require.NoError(t, err)
		require.Empty(t, data)

		return &fs.Part{
			PartNumber: req.PartNumber,
			ETag:       "empty-etag",
			Size:       0,
		}, nil
	}
	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}
	info, err := core.PutObjectPart(ctx, bucketName, objectKey, uploadID, 1,
		bytes.NewReader([]byte{}), 0, minio.PutObjectPartOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(0), info.Size)
}
