package handler_test

import (
	"context"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

func TestListObjects(t *testing.T) {
	const bucketName = "test-bucket"

	expectedObjects := []fs.Object{
		{Key: "file1.txt", Size: 12, LastModified: time.Now()},
		{Key: "file2.txt", Size: 12, LastModified: time.Now()},
		{Key: "dir/file3.txt", Size: 12, LastModified: time.Now()},
	}

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
		ListObjectsFunc: func(ctx context.Context, bucket string, prefix string) ([]fs.Object, error) {
			require.Equal(t, bucketName, bucket)

			if prefix == "" {
				return expectedObjects, nil
			}
			// Filter by prefix
			var filtered []fs.Object

			for _, obj := range expectedObjects {
				if len(obj.Key) >= len(prefix) && obj.Key[:len(prefix)] == prefix {
					filtered = append(filtered, obj)
				}
			}

			return filtered, nil
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// List all objects
	t.Run("ListAll", func(t *testing.T) {
		objectCh := client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{})

		var found []string

		for obj := range objectCh {
			require.NoError(t, obj.Err)
			found = append(found, obj.Key)
		}

		require.Len(t, found, 3)
		require.Contains(t, found, "file1.txt")
		require.Contains(t, found, "file2.txt")
		require.Contains(t, found, "dir/file3.txt")
	})

	// List with prefix
	t.Run("ListWithPrefix", func(t *testing.T) {
		objectCh := client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{
			Prefix: "dir/",
		})

		var found []string

		for obj := range objectCh {
			require.NoError(t, obj.Err)
			found = append(found, obj.Key)
		}

		require.Len(t, found, 1)
		require.Contains(t, found, "dir/file3.txt")
	})
}

func TestListObjects_EmptyBucket(t *testing.T) {
	const bucketName = "empty-bucket"

	svc := &mock.ServiceMock{
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
		ListObjectsFunc: func(ctx context.Context, bucket string, prefix string) ([]fs.Object, error) {
			require.Equal(t, bucketName, bucket)
			return []fs.Object{}, nil
		},
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// List objects in empty bucket
	objectCh := client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{})

	var found []string

	for obj := range objectCh {
		require.NoError(t, obj.Err)
		found = append(found, obj.Key)
	}

	require.Empty(t, found)
}
