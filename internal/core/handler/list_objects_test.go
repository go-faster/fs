package handler_test

import (
	"context"
	"encoding/xml"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/handler"
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

	// Recursive listing returns the flat keyspace.
	t.Run("ListAll", func(t *testing.T) {
		objectCh := client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{Recursive: true})

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

	// Non-recursive (delimited) listing rolls nested keys into common prefixes.
	t.Run("ListDelimited", func(t *testing.T) {
		objectCh := client.ListObjects(ctx, bucketName, minio.ListObjectsOptions{})

		var found []string

		for obj := range objectCh {
			require.NoError(t, obj.Err)
			found = append(found, obj.Key)
		}

		require.Len(t, found, 3)
		require.Contains(t, found, "file1.txt")
		require.Contains(t, found, "file2.txt")
		require.Contains(t, found, "dir/") // common prefix, not dir/file3.txt
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

func TestListObjects_Delimiter(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	for _, k := range []string{"a/1", "a/2", "b/1", "top"} {
		require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/"+k, "x", nil).Code)
	}

	rec := do(t, h, http.MethodGet, "/bucket-a?delimiter=/", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var result handler.ListBucketResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))

	require.Len(t, result.Contents, 1)
	require.Equal(t, "top", result.Contents[0].Key)

	var prefixes []string
	for _, cp := range result.CommonPrefixes {
		prefixes = append(prefixes, cp.Prefix)
	}

	require.ElementsMatch(t, []string{"a/", "b/"}, prefixes)
}

func TestListObjectsV2_Pagination(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	const n = 5
	for i := range n {
		key := "k" + strconv.Itoa(i)
		require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/"+key, "x", nil).Code)
	}

	var (
		seen  []string
		token string
		pages int
	)

	for {
		target := "/bucket-a?list-type=2&max-keys=2"
		if token != "" {
			target += "&continuation-token=" + token
		}

		rec := do(t, h, http.MethodGet, target, "", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var result handler.ListBucketResult
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))

		for _, o := range result.Contents {
			seen = append(seen, o.Key)
		}

		pages++

		require.LessOrEqual(t, len(result.Contents), 2)

		if !result.IsTruncated {
			break
		}

		require.NotEmpty(t, result.NextContinuationToken)
		token = result.NextContinuationToken

		require.Less(t, pages, 10, "pagination did not terminate")
	}

	require.Equal(t, []string{"k0", "k1", "k2", "k3", "k4"}, seen)
	require.Equal(t, 3, pages) // 2 + 2 + 1
}
