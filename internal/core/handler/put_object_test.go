package handler_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// putRequest builds a raw PUT request with the given headers for direct handler testing.
func putRequest(t testing.TB, bucket, key, body string, headers map[string]string) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"/"+key, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return req
}

func TestPutObject_IfNoneMatch_Conflict(t *testing.T) {
	const (
		bucketName = "test-bucket"
		objectKey  = "hello.txt"
	)

	var putCalled bool

	svc := baseMock()
	svc.GetObjectFunc = func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
		require.Equal(t, bucketName, bucket)
		require.Equal(t, objectKey, key)

		// Object already exists.
		return &fs.GetObjectResponse{Reader: io.NopCloser(strings.NewReader("existing"))}, nil
	}
	svc.PutObjectFunc = func(ctx context.Context, req *fs.PutObjectRequest) error {
		putCalled = true
		return nil
	}

	rec := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rec, putRequest(t, bucketName, objectKey, "new", map[string]string{
		"If-None-Match": "*",
	}))

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	require.False(t, putCalled, "object must not be written when it already exists")
}

func TestPutObject_IfNoneMatch_Created(t *testing.T) {
	const (
		bucketName = "test-bucket"
		objectKey  = "hello.txt"
	)

	var putCalled bool

	svc := baseMock()
	svc.GetObjectFunc = func(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
		// Object does not exist yet.
		return nil, fs.ErrObjectNotFound
	}
	svc.PutObjectFunc = func(ctx context.Context, req *fs.PutObjectRequest) error {
		putCalled = true
		require.Equal(t, bucketName, req.Bucket)
		require.Equal(t, objectKey, req.Key)

		return nil
	}

	rec := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rec, putRequest(t, bucketName, objectKey, "new", map[string]string{
		"If-None-Match": "*",
	}))

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, putCalled, "object must be written when it does not exist")
}
