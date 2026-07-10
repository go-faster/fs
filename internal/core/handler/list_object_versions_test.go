package handler_test

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/mock"
)

func TestListObjectVersions(t *testing.T) {
	const bucketName = "test-bucket"

	objects := []fs.Object{
		{Key: "a/b.txt", Size: 2, ETag: "d41d8cd98f00b204e9800998ecf8427e", LastModified: time.Now()},
		{Key: "c.txt", Size: 3, ETag: "0cc175b9c0f1b6a831c399e269772661", LastModified: time.Now()},
	}

	svc := &mock.StorageMock{
		ListObjectsFunc: func(_ context.Context, bucket, _ string) ([]fs.Object, error) {
			require.Equal(t, bucketName, bucket)
			return objects, nil
		},
	}

	h := handler.New(svc)

	req := httptest.NewRequest(http.MethodGet, "/"+bucketName+"?versions", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/xml", rec.Header().Get("Content-Type"))

	var result handler.ListVersionsResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))

	require.Equal(t, bucketName, result.Name)
	require.False(t, result.IsTruncated)
	require.Len(t, result.Versions, 2)

	// Objects are reported as single "null" versions, sorted, marked latest,
	// with quoted ETags — the shape S3 clients expect for an unversioned store.
	require.Equal(t, "a/b.txt", result.Versions[0].Key)
	require.Equal(t, "c.txt", result.Versions[1].Key)

	for _, v := range result.Versions {
		require.Equal(t, "null", v.VersionID)
		require.True(t, v.IsLatest)
		require.NotEmpty(t, v.ETag)
		require.Equal(t, byte('"'), v.ETag[0], "ETag must be quoted")
	}
}

func TestListObjectVersions_BucketNotFound(t *testing.T) {
	svc := &mock.StorageMock{
		ListObjectsFunc: func(context.Context, string, string) ([]fs.Object, error) {
			return nil, fs.ErrBucketNotFound
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/missing?versions", http.NoBody)
	rec := httptest.NewRecorder()
	handler.New(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}
