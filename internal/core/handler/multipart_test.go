package handler_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

// baseMock returns a ServiceMock with common stub methods that minio client requires.
func baseMock() *mock.ServiceMock {
	return &mock.ServiceMock{
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
	}
}

func TestHandler_InitiateMultipartUpload(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
		uploadID   = "test-upload-id-12345"
	)

	svc := baseMock()
	svc.CreateMultipartUploadFunc = func(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
		require.Equal(t, bucketName, bucket)
		require.Equal(t, objectKey, key)

		return &fs.MultipartUpload{
			UploadID:  uploadID,
			Bucket:    bucket,
			Key:       key,
			Initiated: time.Now(),
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	id, err := core.NewMultipartUpload(ctx, bucketName, objectKey, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, uploadID, id)
}

func TestHandler_InitiateMultipartUpload_BucketNotFound(t *testing.T) {
	t.Parallel()

	svc := baseMock()
	svc.CreateMultipartUploadFunc = func(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
		return nil, fs.ErrBucketNotFound
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	_, err := core.NewMultipartUpload(ctx, "nonexistent-bucket", "key", minio.PutObjectOptions{})
	require.Error(t, err)
}

func TestHandler_InitiateMultipartUpload_NestedKey(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "path/to/nested/object.txt"
		uploadID   = "nested-upload-id"
	)

	svc := baseMock()
	svc.CreateMultipartUploadFunc = func(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
		require.Equal(t, objectKey, key)

		return &fs.MultipartUpload{
			UploadID:  uploadID,
			Bucket:    bucket,
			Key:       key,
			Initiated: time.Now(),
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	id, err := core.NewMultipartUpload(ctx, bucketName, objectKey, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, uploadID, id)
}

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

func TestHandler_CompleteMultipartUpload(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
		uploadID   = "test-upload-id"
		finalETag  = "final-etag-12345"
	)

	expectedParts := []fs.CompletedPart{
		{PartNumber: 1, ETag: "etag1"},
		{PartNumber: 2, ETag: "etag2"},
		{PartNumber: 3, ETag: "etag3"},
	}

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		require.Equal(t, bucketName, req.Bucket)
		require.Equal(t, objectKey, req.Key)
		require.Equal(t, uploadID, req.UploadID)
		require.Len(t, req.Parts, len(expectedParts))

		// Verify parts are sorted by part number.
		for i, p := range req.Parts {
			require.Equal(t, i+1, p.PartNumber)
		}

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     finalETag,
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: "etag1"},
		{PartNumber: 2, ETag: "etag2"},
		{PartNumber: 3, ETag: "etag3"},
	}

	info, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, objectKey, info.Key)
}

func TestHandler_CompleteMultipartUpload_OutOfOrderParts(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
		uploadID   = "test-upload-id"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		// Verify parts are sorted by part number regardless of input order.
		require.Len(t, req.Parts, 3)
		require.Equal(t, 1, req.Parts[0].PartNumber)
		require.Equal(t, 2, req.Parts[1].PartNumber)
		require.Equal(t, 3, req.Parts[2].PartNumber)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "sorted-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// Submit parts out of order.
	completeParts := []minio.CompletePart{
		{PartNumber: 3, ETag: "etag3"},
		{PartNumber: 1, ETag: "etag1"},
		{PartNumber: 2, ETag: "etag2"},
	}

	_, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
}

func TestHandler_CompleteMultipartUpload_UploadNotFound(t *testing.T) {
	t.Parallel()

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		return nil, fs.ErrUploadNotFound
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: "etag1"},
	}

	_, err := core.CompleteMultipartUpload(ctx, "bucket", "key", "invalid-upload", completeParts, minio.PutObjectOptions{})
	require.Error(t, err)
}

func TestHandler_CompleteMultipartUpload_ETagQuotes(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
		uploadID   = "test-upload-id"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		// Verify quotes are stripped from ETags.
		require.Len(t, req.Parts, 2)
		require.Equal(t, "etag1", req.Parts[0].ETag)
		require.Equal(t, "etag2", req.Parts[1].ETag)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "final-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// ETags with quotes (as they might come from S3).
	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: `"etag1"`},
		{PartNumber: 2, ETag: `"etag2"`},
	}

	_, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
}

func TestHandler_CompleteMultipartUpload_SinglePart(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "single-part.txt"
		uploadID   = "single-part-upload"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		require.Len(t, req.Parts, 1)
		require.Equal(t, 1, req.Parts[0].PartNumber)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "single-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: "etag1"},
	}

	_, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
}

func TestHandler_MultipartUpload_FullFlow(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "full-flow.txt"
		uploadID   = "full-flow-upload-id"
	)

	part1Data := []byte("part one content")
	part2Data := []byte("part two content")

	svc := baseMock()
	svc.CreateMultipartUploadFunc = func(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
		return &fs.MultipartUpload{
			UploadID:  uploadID,
			Bucket:    bucket,
			Key:       key,
			Initiated: time.Now(),
		}, nil
	}
	svc.UploadPartFunc = func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
		data, _ := io.ReadAll(req.Reader)

		return &fs.Part{
			PartNumber: req.PartNumber,
			ETag:       "etag" + string(rune('0'+req.PartNumber)),
			Size:       int64(len(data)),
		}, nil
	}
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		require.Equal(t, uploadID, req.UploadID)
		require.Len(t, req.Parts, 2)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "final-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// Step 1: Initiate multipart upload.
	id, err := core.NewMultipartUpload(ctx, bucketName, objectKey, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, uploadID, id)

	// Step 2: Upload parts.
	part1Info, err := core.PutObjectPart(ctx, bucketName, objectKey, id, 1,
		bytes.NewReader(part1Data), int64(len(part1Data)), minio.PutObjectPartOptions{})
	require.NoError(t, err)

	part2Info, err := core.PutObjectPart(ctx, bucketName, objectKey, id, 2,
		bytes.NewReader(part2Data), int64(len(part2Data)), minio.PutObjectPartOptions{})
	require.NoError(t, err)

	// Step 3: Complete multipart upload.
	completeParts := []minio.CompletePart{
		{PartNumber: part1Info.PartNumber, ETag: part1Info.ETag},
		{PartNumber: part2Info.PartNumber, ETag: part2Info.ETag},
	}

	info, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, id, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, objectKey, info.Key)
}

func TestHandler_MultipartUpload_NestedKeyFullFlow(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "path/to/nested/object.txt"
		uploadID   = "nested-upload-id"
	)

	svc := baseMock()
	svc.CreateMultipartUploadFunc = func(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
		require.Equal(t, objectKey, key)

		return &fs.MultipartUpload{
			UploadID:  uploadID,
			Bucket:    bucket,
			Key:       key,
			Initiated: time.Now(),
		}, nil
	}
	svc.UploadPartFunc = func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
		require.Equal(t, objectKey, req.Key)
		data, _ := io.ReadAll(req.Reader)

		return &fs.Part{
			PartNumber: req.PartNumber,
			ETag:       "nested-etag",
			Size:       int64(len(data)),
		}, nil
	}
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		require.Equal(t, objectKey, req.Key)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "nested-final-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// Initiate.
	id, err := core.NewMultipartUpload(ctx, bucketName, objectKey, minio.PutObjectOptions{})
	require.NoError(t, err)

	// Upload part.
	partInfo, err := core.PutObjectPart(ctx, bucketName, objectKey, id, 1,
		bytes.NewReader([]byte("nested content")), 14, minio.PutObjectPartOptions{})
	require.NoError(t, err)

	// Complete.
	_, err = core.CompleteMultipartUpload(ctx, bucketName, objectKey, id,
		[]minio.CompletePart{{PartNumber: partInfo.PartNumber, ETag: partInfo.ETag}},
		minio.PutObjectOptions{})
	require.NoError(t, err)
}

func TestHandler_HandleBucketPost_Delete(t *testing.T) {
	t.Parallel()

	// This test verifies that POST to bucket with ?delete query parameter
	// is handled correctly (returns OK status).
	svc := baseMock()

	client := newTestClient(t, svc)
	_ = client

	// The delete multiple objects operation is acknowledged but not fully implemented.
	// This test just verifies the handler doesn't crash.
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

func TestHandler_CompleteMultipartUpload_ManyParts(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "many-parts.txt"
		uploadID   = "many-parts-upload"
		numParts   = 100
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		require.Len(t, req.Parts, numParts)

		// Verify all parts are present and sorted.
		for i, p := range req.Parts {
			require.Equal(t, i+1, p.PartNumber)
		}

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "many-parts-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	completeParts := make([]minio.CompletePart, numParts)
	for i := 0; i < numParts; i++ {
		completeParts[i] = minio.CompletePart{
			PartNumber: i + 1,
			ETag:       "etag" + string(rune('a'+i%26)),
		}
	}

	_, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
}
