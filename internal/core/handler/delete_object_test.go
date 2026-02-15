package handler_test

import (
	"context"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestHandler_DeleteObject(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
	)

	svc := &mock.ServiceMock{
		DeleteObjectFunc: func(ctx context.Context, bucket, key string) error {
			require.Equal(t, bucketName, bucket)
			require.Equal(t, objectKey, key)

			return nil
		},
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
	}
	ctx := t.Context()
	client := newTestClient(t, svc)
	err := client.RemoveObject(ctx, bucketName, objectKey, minio.RemoveObjectOptions{})
	require.NoError(t, err)
}

func TestHandler_DeleteObject_NotFound(t *testing.T) {
	t.Parallel()

	svc := &mock.ServiceMock{
		DeleteObjectFunc: func(ctx context.Context, bucket, key string) error {
			return fs.ErrObjectNotFound
		},
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
	}
	ctx := t.Context()
	client := newTestClient(t, svc)
	err := client.RemoveObject(ctx, "test-bucket", "nonexistent.txt", minio.RemoveObjectOptions{})
	require.Error(t, err)
}

func TestHandler_AbortMultipartUpload(t *testing.T) {
	t.Parallel()

	svc := baseMock()
	uploadID := ""
	svc.CreateMultipartUploadFunc = func(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
		return &fs.MultipartUpload{
			UploadID: "test-upload-123",
			Bucket:   bucket,
			Key:      key,
		}, nil
	}
	svc.AbortMultipartUploadFunc = func(ctx context.Context, bucket, key, uID string) error {
		require.Equal(t, "test-bucket", bucket)
		require.Equal(t, "test-key.txt", key)
		require.Equal(t, uploadID, uID)

		return nil
	}
	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}
	// Initiate upload.
	uID, err := core.NewMultipartUpload(ctx, "test-bucket", "test-key.txt", minio.PutObjectOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, uID)
	uploadID = uID
	// Abort upload.
	err = core.AbortMultipartUpload(ctx, "test-bucket", "test-key.txt", uploadID)
	require.NoError(t, err)
}

func TestHandler_AbortMultipartUpload_NotFound(t *testing.T) {
	t.Parallel()

	svc := baseMock()
	svc.AbortMultipartUploadFunc = func(ctx context.Context, bucket, key, uploadID string) error {
		return fs.ErrUploadNotFound
	}
	ctx := t.Context()
	core := minio.Core{Client: newTestClient(t, svc)}
	err := core.AbortMultipartUpload(ctx, "test-bucket", "test-key.txt", "invalid-upload-id")
	require.Error(t, err)
}
