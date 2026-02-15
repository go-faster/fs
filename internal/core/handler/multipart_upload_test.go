package handler_test

import (
	"context"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

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
