package handler_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestPutObject(t *testing.T) {
	const (
		bucketName = "test-bucket"
		objectKey  = "hello.txt"
	)
	expectedContent := []byte("Hello, World!")

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
		ListObjectsFunc: func(ctx context.Context, bucket string, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		PutObjectFunc: func(ctx context.Context, req *fs.PutObjectRequest) error {
			require.Equal(t, bucketName, req.Bucket)
			require.Equal(t, objectKey, req.Key)

			return nil
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Put object
	reader := bytes.NewReader(expectedContent)
	_, err := client.PutObject(ctx, bucketName, objectKey, reader, int64(len(expectedContent)), minio.PutObjectOptions{})
	require.NoError(t, err)
}
