package handler_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestHandler_HeadObject(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "test-object.txt"
	)

	lastModified := time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)

	svc := &mock.ServiceMock{
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			require.Equal(t, bucketName, bucket)
			require.Equal(t, objectKey, key)

			return &fs.GetObjectResponse{
				Reader:       io.NopCloser(bytes.NewReader([]byte("content"))),
				Size:         7,
				LastModified: lastModified,
				ETag:         "abc123",
				ContentType:  "text/plain",
			}, nil
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

	info, err := client.StatObject(ctx, bucketName, objectKey, minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(7), info.Size)
	require.Equal(t, "abc123", info.ETag)
	require.Equal(t, "text/plain", info.ContentType)
}

func TestHandler_HeadObject_NotFound(t *testing.T) {
	t.Parallel()

	svc := &mock.ServiceMock{
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			return nil, fs.ErrObjectNotFound
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

	_, err := client.StatObject(ctx, "test-bucket", "nonexistent.txt", minio.StatObjectOptions{})
	require.Error(t, err)
}

func TestHandler_HeadObject_BucketNotFound(t *testing.T) {
	t.Parallel()

	svc := &mock.ServiceMock{
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			return nil, fs.ErrBucketNotFound
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

	_, err := client.StatObject(ctx, "nonexistent-bucket", "test.txt", minio.StatObjectOptions{})
	require.Error(t, err)
}

func TestHandler_HeadObject_NoETag(t *testing.T) {
	t.Parallel()

	lastModified := time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)

	svc := &mock.ServiceMock{
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			return &fs.GetObjectResponse{
				Reader:       io.NopCloser(bytes.NewReader([]byte("content"))),
				Size:         7,
				LastModified: lastModified,
				ETag:         "",
				ContentType:  "application/octet-stream",
			}, nil
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

	info, err := client.StatObject(ctx, "test-bucket", "test.txt", minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(7), info.Size)
}

func TestHandler_HeadObject_NoContentType(t *testing.T) {
	t.Parallel()

	lastModified := time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)

	svc := &mock.ServiceMock{
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			return &fs.GetObjectResponse{
				Reader:       io.NopCloser(bytes.NewReader([]byte("content"))),
				Size:         7,
				LastModified: lastModified,
				ETag:         "abc123",
				ContentType:  "",
			}, nil
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

	info, err := client.StatObject(ctx, "test-bucket", "test.txt", minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(7), info.Size)
	require.Equal(t, "abc123", info.ETag)
}

func TestHandler_HeadObject_NestedKey(t *testing.T) {
	t.Parallel()

	const objectKey = "path/to/nested/file.txt"
	lastModified := time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)

	svc := &mock.ServiceMock{
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			require.Equal(t, objectKey, key)

			return &fs.GetObjectResponse{
				Reader:       io.NopCloser(bytes.NewReader([]byte("nested content"))),
				Size:         14,
				LastModified: lastModified,
				ETag:         "nested-etag",
				ContentType:  "text/plain",
			}, nil
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

	info, err := client.StatObject(ctx, "test-bucket", objectKey, minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(14), info.Size)
	require.Equal(t, "nested-etag", info.ETag)
}

func TestHandler_HeadObject_LargeFile(t *testing.T) {
	t.Parallel()

	const fileSize = 10 * 1024 * 1024 * 1024 // 10GB
	lastModified := time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)

	svc := &mock.ServiceMock{
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			return &fs.GetObjectResponse{
				Reader:       io.NopCloser(bytes.NewReader(nil)),
				Size:         fileSize,
				LastModified: lastModified,
				ETag:         "large-file-etag",
				ContentType:  "application/octet-stream",
			}, nil
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

	info, err := client.StatObject(ctx, "test-bucket", "large-file.bin", minio.StatObjectOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(fileSize), info.Size)
}

func TestHandler_HeadObject_InvalidBucketName(t *testing.T) {
	t.Parallel()

	svc := &mock.ServiceMock{
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			return nil, fs.ErrInvalidBucketName
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

	_, err := client.StatObject(ctx, "test-bucket", "test.txt", minio.StatObjectOptions{})
	require.Error(t, err)
}
