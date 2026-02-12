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

func TestGetObject(t *testing.T) {
	const (
		bucketName = "test-bucket"
		objectKey  = "hello.txt"
	)

	expectedContent := []byte("Hello, World!")
	expectedTime := time.Now().Truncate(time.Second)

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			require.Equal(t, bucketName, bucket)
			require.Equal(t, objectKey, key)

			return &fs.GetObjectResponse{
				Reader:       io.NopCloser(bytes.NewReader(expectedContent)),
				Size:         int64(len(expectedContent)),
				LastModified: expectedTime,
				ContentType:  "text/plain",
			}, nil
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	obj, err := client.GetObject(ctx, bucketName, objectKey, minio.GetObjectOptions{})
	require.NoError(t, err)

	content, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, expectedContent, content)

	info, err := obj.Stat()
	require.NoError(t, err)
	require.Equal(t, int64(len(expectedContent)), info.Size)
}

func TestGetObject_NotFound(t *testing.T) {
	const (
		bucketName = "test-bucket"
		objectKey  = "nonexistent.txt"
	)

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			return nil, fs.ErrObjectNotFound
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	obj, err := client.GetObject(ctx, bucketName, objectKey, minio.GetObjectOptions{})
	require.NoError(t, err)

	_, err = obj.Stat()
	require.Error(t, err)
}

func TestGetObject_BucketNotFound(t *testing.T) {
	const (
		bucketName = "nonexistent-bucket"
		objectKey  = "hello.txt"
	)

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			return nil, fs.ErrBucketNotFound
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	obj, err := client.GetObject(ctx, bucketName, objectKey, minio.GetObjectOptions{})
	require.NoError(t, err)

	_, err = obj.Stat()
	require.Error(t, err)
}

func TestGetObject_NestedKey(t *testing.T) {
	const (
		bucketName = "test-bucket"
		objectKey  = "dir/subdir/file.txt"
	)

	expectedContent := []byte("Nested content")

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		GetObjectFunc: func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
			require.Equal(t, bucketName, bucket)
			require.Equal(t, objectKey, key)

			return &fs.GetObjectResponse{
				Reader:       io.NopCloser(bytes.NewReader(expectedContent)),
				Size:         int64(len(expectedContent)),
				LastModified: time.Now(),
			}, nil
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	obj, err := client.GetObject(ctx, bucketName, objectKey, minio.GetObjectOptions{})
	require.NoError(t, err)

	content, err := io.ReadAll(obj)
	require.NoError(t, err)
	require.Equal(t, expectedContent, content)
}
