package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/internal/mock"
)

func TestService_ListBuckets(t *testing.T) {
	expectedBuckets := []fs.Bucket{
		{Name: "bucket1"},
		{Name: "bucket2"},
	}

	storage := &mock.StorageMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return expectedBuckets, nil
		},
	}

	svc := service.New(storage)
	ctx := t.Context()

	buckets, err := svc.ListBuckets(ctx)
	require.NoError(t, err)
	require.Equal(t, expectedBuckets, buckets)
}

func TestService_CreateBucket(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		storage := &mock.StorageMock{
			CreateBucketFunc: func(ctx context.Context, bucket string) error {
				require.Equal(t, "valid-bucket", bucket)
				return nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.CreateBucket(ctx, "valid-bucket")
		require.NoError(t, err)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.CreateBucket(ctx, "INVALID")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})
}

func TestService_DeleteBucket(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		storage := &mock.StorageMock{
			DeleteBucketFunc: func(ctx context.Context, bucket string) error {
				require.Equal(t, "valid-bucket", bucket)
				return nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.DeleteBucket(ctx, "valid-bucket")
		require.NoError(t, err)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.DeleteBucket(ctx, "INVALID")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})
}

func TestService_ListObjects(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		expectedObjects := []fs.Object{
			{Key: "file1.txt", Size: 100},
			{Key: "file2.txt", Size: 200},
		}

		storage := &mock.StorageMock{
			ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
				require.Equal(t, "valid-bucket", bucket)
				require.Equal(t, "prefix/", prefix)

				return expectedObjects, nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		objects, err := svc.ListObjects(ctx, "valid-bucket", "prefix/")
		require.NoError(t, err)
		require.Equal(t, expectedObjects, objects)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.ListObjects(ctx, "INVALID", "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})

	t.Run("InvalidPrefix", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.ListObjects(ctx, "valid-bucket", "invalid\x00prefix")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate prefix")
	})
}

func TestService_PutObject(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		storage := &mock.StorageMock{
			PutObjectFunc: func(ctx context.Context, req *fs.PutObjectRequest) error {
				require.Equal(t, "valid-bucket", req.Bucket)
				require.Equal(t, "valid-key.txt", req.Key)

				return nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.PutObject(ctx, &fs.PutObjectRequest{
			Bucket: "valid-bucket",
			Key:    "valid-key.txt",
		})
		require.NoError(t, err)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.PutObject(ctx, &fs.PutObjectRequest{
			Bucket: "INVALID",
			Key:    "valid-key.txt",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})

	t.Run("InvalidKey", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.PutObject(ctx, &fs.PutObjectRequest{
			Bucket: "valid-bucket",
			Key:    "",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate object key")
	})
}

func TestService_DeleteObject(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		storage := &mock.StorageMock{
			DeleteObjectFunc: func(ctx context.Context, bucket, key string) error {
				require.Equal(t, "valid-bucket", bucket)
				require.Equal(t, "valid-key.txt", key)

				return nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.DeleteObject(ctx, "valid-bucket", "valid-key.txt")
		require.NoError(t, err)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.DeleteObject(ctx, "INVALID", "valid-key.txt")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})

	t.Run("InvalidKey", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.DeleteObject(ctx, "valid-bucket", "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate object key")
	})
}
