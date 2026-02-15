package handler_test

import (
	"context"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestHandler_HeadBucket(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	svc := &mock.ServiceMock{
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			require.Equal(t, bucketName, bucket)
			return []fs.Object{}, nil
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
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return nil, fs.ErrBucketNotFound
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	exists, err := client.BucketExists(ctx, bucketName)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestHandler_HeadBucket_InternalError(t *testing.T) {
	t.Parallel()

	svc := &mock.ServiceMock{
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return nil, errors.New("database connection failed")
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// BucketExists returns error for 500 errors.
	_, err := client.BucketExists(ctx, "some-bucket")
	require.Error(t, err)
}

func TestHandler_HeadBucket_WithObjects(t *testing.T) {
	t.Parallel()

	const bucketName = "bucket-with-objects"

	svc := &mock.ServiceMock{
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			require.Equal(t, bucketName, bucket)

			return []fs.Object{
				{Key: "file1.txt", Size: 100},
				{Key: "file2.txt", Size: 200},
			}, nil
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	exists, err := client.BucketExists(ctx, bucketName)
	require.NoError(t, err)
	require.True(t, exists)
}
