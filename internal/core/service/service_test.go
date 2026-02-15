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

func TestService_GetObject(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		expectedResp := &fs.GetObjectResponse{
			Size: 100,
			ETag: "abc123",
		}

		storage := &mock.StorageMock{
			GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
				require.Equal(t, "valid-bucket", bucket)
				require.Equal(t, "valid-key.txt", key)

				return expectedResp, nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		resp, err := svc.GetObject(ctx, "valid-bucket", "valid-key.txt")
		require.NoError(t, err)
		require.Equal(t, expectedResp, resp)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.GetObject(ctx, "INVALID", "valid-key.txt")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})

	t.Run("InvalidKey", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.GetObject(ctx, "valid-bucket", "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate object key")
	})
}

func TestService_BucketExists(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		storage := &mock.StorageMock{
			BucketExistsFunc: func(ctx context.Context, bucket string) (bool, error) {
				require.Equal(t, "valid-bucket", bucket)
				return true, nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		exists, err := svc.BucketExists(ctx, "valid-bucket")
		require.NoError(t, err)
		require.True(t, exists)
	})

	t.Run("NotFound", func(t *testing.T) {
		storage := &mock.StorageMock{
			BucketExistsFunc: func(ctx context.Context, bucket string) (bool, error) {
				return false, nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		exists, err := svc.BucketExists(ctx, "nonexistent-bucket")
		require.NoError(t, err)
		require.False(t, exists)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.BucketExists(ctx, "INVALID")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})
}

func TestService_CreateMultipartUpload(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		expectedUpload := &fs.MultipartUpload{
			UploadID: "upload-123",
			Bucket:   "valid-bucket",
			Key:      "valid-key.txt",
		}

		storage := &mock.StorageMock{
			CreateMultipartUploadFunc: func(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
				require.Equal(t, "valid-bucket", bucket)
				require.Equal(t, "valid-key.txt", key)

				return expectedUpload, nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		upload, err := svc.CreateMultipartUpload(ctx, "valid-bucket", "valid-key.txt")
		require.NoError(t, err)
		require.Equal(t, expectedUpload, upload)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.CreateMultipartUpload(ctx, "INVALID", "valid-key.txt")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})

	t.Run("InvalidKey", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.CreateMultipartUpload(ctx, "valid-bucket", "")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate object key")
	})
}

func TestService_UploadPart(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		expectedPart := &fs.Part{
			PartNumber: 1,
			ETag:       "etag-123",
			Size:       1024,
		}

		storage := &mock.StorageMock{
			UploadPartFunc: func(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
				require.Equal(t, "valid-bucket", req.Bucket)
				require.Equal(t, "valid-key.txt", req.Key)
				require.Equal(t, 1, req.PartNumber)

				return expectedPart, nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		part, err := svc.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket:     "valid-bucket",
			Key:        "valid-key.txt",
			PartNumber: 1,
		})
		require.NoError(t, err)
		require.Equal(t, expectedPart, part)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket:     "INVALID",
			Key:        "valid-key.txt",
			PartNumber: 1,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})

	t.Run("InvalidKey", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket:     "valid-bucket",
			Key:        "",
			PartNumber: 1,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate object key")
	})
}

func TestService_CompleteMultipartUpload(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		expectedResp := &fs.CompleteMultipartUploadResponse{
			Location: "/valid-bucket/valid-key.txt",
			Bucket:   "valid-bucket",
			Key:      "valid-key.txt",
			ETag:     "final-etag",
		}

		storage := &mock.StorageMock{
			CompleteMultipartUploadFunc: func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
				require.Equal(t, "valid-bucket", req.Bucket)
				require.Equal(t, "valid-key.txt", req.Key)
				require.Equal(t, "upload-123", req.UploadID)

				return expectedResp, nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		resp, err := svc.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
			Bucket:   "valid-bucket",
			Key:      "valid-key.txt",
			UploadID: "upload-123",
			Parts: []fs.CompletedPart{
				{PartNumber: 1, ETag: "etag1"},
			},
		})
		require.NoError(t, err)
		require.Equal(t, expectedResp, resp)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
			Bucket:   "INVALID",
			Key:      "valid-key.txt",
			UploadID: "upload-123",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})

	t.Run("InvalidKey", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		_, err := svc.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
			Bucket:   "valid-bucket",
			Key:      "",
			UploadID: "upload-123",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate object key")
	})
}

func TestService_AbortMultipartUpload(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		storage := &mock.StorageMock{
			AbortMultipartUploadFunc: func(ctx context.Context, bucket, key, uploadID string) error {
				require.Equal(t, "valid-bucket", bucket)
				require.Equal(t, "valid-key.txt", key)
				require.Equal(t, "upload-123", uploadID)

				return nil
			},
		}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.AbortMultipartUpload(ctx, "valid-bucket", "valid-key.txt", "upload-123")
		require.NoError(t, err)
	})

	t.Run("InvalidBucketName", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.AbortMultipartUpload(ctx, "INVALID", "valid-key.txt", "upload-123")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate bucket name")
	})

	t.Run("InvalidKey", func(t *testing.T) {
		storage := &mock.StorageMock{}

		svc := service.New(storage)
		ctx := t.Context()

		err := svc.AbortMultipartUpload(ctx, "valid-bucket", "", "upload-123")
		require.Error(t, err)
		require.Contains(t, err.Error(), "validate object key")
	})
}
