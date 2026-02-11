package handler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestHandler_ListBuckets(t *testing.T) {
	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{
				{Name: "bucket1"},
				{Name: "bucket2"},
			}, nil
		},
	}

	ctx := t.Context()
	buckets, err := newTestClient(t, svc).ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 2)
	require.Equal(t, "bucket1", buckets[0].Name)
	require.Equal(t, "bucket2", buckets[1].Name)
}

func BenchmarkHandler_ListBuckets(b *testing.B) {
	b.ReportAllocs()

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{
				{Name: "bucket1"},
				{Name: "bucket2"},
			}, nil
		},
	}

	ctx := context.Background()
	client := newTestClient(b, svc)

	b.ResetTimer()
	for b.Loop() {
		_, err := client.ListBuckets(ctx)
		require.NoError(b, err)
	}
}
