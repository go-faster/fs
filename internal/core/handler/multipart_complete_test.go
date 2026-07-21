package handler_test

import (
	"context"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

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

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		t.Fatal("storage must not be called for an out-of-order part list")
		return nil, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// Out-of-order part lists are rejected with InvalidPartOrder, like on S3.
	completeParts := []minio.CompletePart{
		{PartNumber: 3, ETag: "etag3"},
		{PartNumber: 1, ETag: "etag1"},
		{PartNumber: 2, ETag: "etag2"},
	}

	_, err := core.CompleteMultipartUpload(ctx, "test-bucket", "test-object.txt", "test-upload-id", completeParts, minio.PutObjectOptions{})
	require.Error(t, err)

	var minioErr minio.ErrorResponse
	require.ErrorAs(t, err, &minioErr)
	require.Equal(t, "InvalidPartOrder", minioErr.Code)
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

func TestHandler_CompleteMultipartUpload_EmptyParts(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "empty-parts.txt"
		uploadID   = "empty-parts-upload"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		t.Fatal("storage must not be called for an empty part list")
		return nil, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// An empty parts list is rejected with MalformedXML, like on S3.
	completeParts := []minio.CompletePart{}

	_, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.Error(t, err)

	var minioErr minio.ErrorResponse
	require.ErrorAs(t, err, &minioErr)
	require.Equal(t, "MalformedXML", minioErr.Code)
}

func TestHandler_CompleteMultipartUpload_NestedKey(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "path/to/nested/object.txt"
		uploadID   = "nested-upload-id"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		require.Equal(t, objectKey, req.Key)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "nested-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: "etag1"},
	}

	info, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, objectKey, info.Key)
}

func TestHandler_CompleteMultipartUpload_DuplicatePartNumbers(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "duplicate-parts.txt"
		uploadID   = "duplicate-upload"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		t.Fatal("storage must not be called for duplicate part numbers")
		return nil, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// Duplicate part numbers violate strict ascending order.
	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: "etag1a"},
		{PartNumber: 1, ETag: "etag1b"},
		{PartNumber: 2, ETag: "etag2a"},
	}

	_, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.Error(t, err)

	var minioErr minio.ErrorResponse
	require.ErrorAs(t, err, &minioErr)
	require.Equal(t, "InvalidPartOrder", minioErr.Code)
}

func TestHandler_CompleteMultipartUpload_LargePartNumbers(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "large-part-numbers.txt"
		uploadID   = "large-part-upload"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		require.Len(t, req.Parts, 3)

		// Verify sorting works with large part numbers.
		require.Equal(t, 1000, req.Parts[0].PartNumber)
		require.Equal(t, 5000, req.Parts[1].PartNumber)
		require.Equal(t, 10000, req.Parts[2].PartNumber)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "large-part-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// Submit parts with large, non-contiguous part numbers in ascending order.
	completeParts := []minio.CompletePart{
		{PartNumber: 1000, ETag: "etag1000"},
		{PartNumber: 5000, ETag: "etag5000"},
		{PartNumber: 10000, ETag: "etag10000"},
	}

	_, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
}

func TestHandler_CompleteMultipartUpload_ServiceError(t *testing.T) {
	t.Parallel()

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		return nil, fs.ErrBucketNotFound
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: "etag1"},
	}

	_, err := core.CompleteMultipartUpload(ctx, "nonexistent-bucket", "key", "upload-id", completeParts, minio.PutObjectOptions{})
	require.Error(t, err)
}

func TestHandler_CompleteMultipartUpload_SpecialCharactersInKey(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "special/chars/file-name_with.dots.txt"
		uploadID   = "special-upload"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		require.Equal(t, objectKey, req.Key)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "special-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: "etag1"},
	}

	info, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, objectKey, info.Key)
}

func TestHandler_CompleteMultipartUpload_ETagWithMixedQuotes(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "mixed-quotes.txt"
		uploadID   = "mixed-upload"
	)

	svc := baseMock()
	svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
		// Verify quotes are properly stripped from all ETags.
		require.Len(t, req.Parts, 3)
		require.Equal(t, "etag1", req.Parts[0].ETag)
		require.Equal(t, "etag2", req.Parts[1].ETag)
		require.Equal(t, "etag3", req.Parts[2].ETag)

		return &fs.CompleteMultipartUploadResponse{
			Location: "/" + bucketName + "/" + objectKey,
			Bucket:   bucketName,
			Key:      objectKey,
			ETag:     "mixed-etag",
		}, nil
	}

	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}

	// Mix of quoted and unquoted ETags.
	completeParts := []minio.CompletePart{
		{PartNumber: 1, ETag: `"etag1"`},
		{PartNumber: 2, ETag: "etag2"},
		{PartNumber: 3, ETag: `"etag3"`},
	}

	_, err := core.CompleteMultipartUpload(ctx, bucketName, objectKey, uploadID, completeParts, minio.PutObjectOptions{})
	require.NoError(t, err)
}
