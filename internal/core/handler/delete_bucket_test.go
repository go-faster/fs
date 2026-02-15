package handler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestHandler_DeleteBucket(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	svc := &mock.ServiceMock{
		DeleteBucketFunc: func(ctx context.Context, bucket string) error {
			require.Equal(t, bucketName, bucket)
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

	err := client.RemoveBucket(ctx, bucketName)
	require.NoError(t, err)
}

func TestHandler_DeleteBucket_NotFound(t *testing.T) {
	t.Parallel()

	svc := &mock.ServiceMock{
		DeleteBucketFunc: func(ctx context.Context, bucket string) error {
			return fs.ErrBucketNotFound
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

	err := client.RemoveBucket(ctx, "nonexistent-bucket")
	require.Error(t, err)
}
