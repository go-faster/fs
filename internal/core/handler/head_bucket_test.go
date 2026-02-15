package handler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestHandler_HeadBucket(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	svc := &mock.ServiceMock{
		BucketExistsFunc: func(ctx context.Context, bucket string) (bool, error) {
			require.Equal(t, bucketName, bucket)
			return true, nil
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

	exists, err := client.BucketExists(ctx, bucketName)
	require.NoError(t, err)
	require.True(t, exists)
}

func TestHandler_HeadBucket_NotFound(t *testing.T) {
	t.Parallel()

	const bucketName = "nonexistent-bucket"

	svc := &mock.ServiceMock{
		BucketExistsFunc: func(ctx context.Context, bucket string) (bool, error) {
			return false, nil
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

	exists, err := client.BucketExists(ctx, bucketName)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestHandler_HeadBucket_InvalidBucketName(t *testing.T) {
	t.Parallel()

	svc := &mock.ServiceMock{
		BucketExistsFunc: func(ctx context.Context, bucket string) (bool, error) {
			return false, fs.ErrInvalidBucketName
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

	// BucketExists returns error for invalid bucket names.
	_, err := client.BucketExists(ctx, "some-bucket")
	require.Error(t, err)
}
