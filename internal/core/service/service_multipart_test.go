package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/internal/mock"
)

// partsStorage returns a mock whose ListParts reports the given uploaded parts
// and whose CompleteMultipartUpload succeeds.
func partsStorage(parts ...fs.Part) *mock.StorageMock {
	return &mock.StorageMock{
		ListPartsFunc: func(ctx context.Context, bucket, key, uploadID string) ([]fs.Part, error) {
			return parts, nil
		},
		CompleteMultipartUploadFunc: func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
			return &fs.CompleteMultipartUploadResponse{Bucket: req.Bucket, Key: req.Key, ETag: "etag"}, nil
		},
	}
}

func completeReq(parts ...fs.CompletedPart) *fs.CompleteMultipartUploadRequest {
	return &fs.CompleteMultipartUploadRequest{
		Bucket:   "valid-bucket",
		Key:      "valid-key.txt",
		UploadID: "upload-123",
		Parts:    parts,
	}
}

func TestService_CompleteMultipartUpload_Validation(t *testing.T) {
	ctx := t.Context()

	bigEnough := int64(service.MinPartSize)

	t.Run("OutOfOrder", func(t *testing.T) {
		svc := service.New(partsStorage(
			fs.Part{PartNumber: 1, ETag: "etag1", Size: bigEnough},
			fs.Part{PartNumber: 2, ETag: "etag2", Size: bigEnough},
		))

		_, err := svc.CompleteMultipartUpload(ctx, completeReq(
			fs.CompletedPart{PartNumber: 2, ETag: "etag2"},
			fs.CompletedPart{PartNumber: 1, ETag: "etag1"},
		))
		require.ErrorIs(t, err, fs.ErrInvalidPartOrder)
	})

	t.Run("DuplicatePartNumber", func(t *testing.T) {
		svc := service.New(partsStorage(fs.Part{PartNumber: 1, ETag: "etag1", Size: bigEnough}))

		_, err := svc.CompleteMultipartUpload(ctx, completeReq(
			fs.CompletedPart{PartNumber: 1, ETag: "etag1"},
			fs.CompletedPart{PartNumber: 1, ETag: "etag1"},
		))
		require.ErrorIs(t, err, fs.ErrInvalidPartOrder)
	})

	t.Run("EmptyParts", func(t *testing.T) {
		svc := service.New(partsStorage())

		_, err := svc.CompleteMultipartUpload(ctx, completeReq())
		require.ErrorIs(t, err, fs.ErrInvalidPart)
	})

	t.Run("UnknownPart", func(t *testing.T) {
		svc := service.New(partsStorage(fs.Part{PartNumber: 1, ETag: "etag1", Size: bigEnough}))

		_, err := svc.CompleteMultipartUpload(ctx, completeReq(
			fs.CompletedPart{PartNumber: 1, ETag: "etag1"},
			fs.CompletedPart{PartNumber: 2, ETag: "etag2"},
		))
		require.ErrorIs(t, err, fs.ErrInvalidPart)
	})

	t.Run("ETagMismatch", func(t *testing.T) {
		svc := service.New(partsStorage(fs.Part{PartNumber: 1, ETag: "etag1", Size: bigEnough}))

		_, err := svc.CompleteMultipartUpload(ctx, completeReq(
			fs.CompletedPart{PartNumber: 1, ETag: "different"},
		))
		require.ErrorIs(t, err, fs.ErrInvalidPart)
	})

	t.Run("NonLastPartTooSmall", func(t *testing.T) {
		svc := service.New(partsStorage(
			fs.Part{PartNumber: 1, ETag: "etag1", Size: 10},
			fs.Part{PartNumber: 2, ETag: "etag2", Size: 10},
		))

		_, err := svc.CompleteMultipartUpload(ctx, completeReq(
			fs.CompletedPart{PartNumber: 1, ETag: "etag1"},
			fs.CompletedPart{PartNumber: 2, ETag: "etag2"},
		))
		require.ErrorIs(t, err, fs.ErrEntityTooSmall)
	})

	t.Run("SmallSinglePartAllowed", func(t *testing.T) {
		svc := service.New(partsStorage(fs.Part{PartNumber: 1, ETag: "etag1", Size: 10}))

		_, err := svc.CompleteMultipartUpload(ctx, completeReq(
			fs.CompletedPart{PartNumber: 1, ETag: "etag1"},
		))
		require.NoError(t, err)
	})
}

func TestService_UploadPart_PartNumberRange(t *testing.T) {
	ctx := t.Context()

	storage := &mock.StorageMock{
		UploadPartFunc: func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
			return &fs.Part{PartNumber: req.PartNumber, ETag: "etag"}, nil
		},
	}
	svc := service.New(storage)

	upload := func(partNumber int) error {
		_, err := svc.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket:     "valid-bucket",
			Key:        "valid-key.txt",
			UploadID:   "upload-123",
			PartNumber: partNumber,
			Reader:     strings.NewReader("data"),
			Size:       4,
		})

		return err
	}

	require.ErrorIs(t, upload(0), fs.ErrInvalidPartNumber)
	require.ErrorIs(t, upload(-1), fs.ErrInvalidPartNumber)
	require.ErrorIs(t, upload(10001), fs.ErrInvalidPartNumber)
	require.NoError(t, upload(1))
	require.NoError(t, upload(10000))
}
