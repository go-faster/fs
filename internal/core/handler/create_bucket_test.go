package handler_test

import (
	"context"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestHandler_CreateBucket(t *testing.T) {
	const expectedBucketName = "bucket1"

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			// ListBucket is called by client.
			// Ignoring.
			return []fs.Bucket{}, nil
		},
		CreateBucketFunc: func(ctx context.Context, bucket string) error {
			require.Equal(t, expectedBucketName, bucket)
			return nil
		},
	}

	ctx := t.Context()
	err := newTestClient(t, svc).MakeBucket(ctx, expectedBucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)
}
