package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/mock"
)

// baseMock returns a ServiceMock with common stub methods that minio client requires.
func baseMock() *mock.ServiceMock {
	return &mock.ServiceMock{
		ListObjectsFunc: func(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
			return []fs.Object{}, nil
		},
		ListBucketsFunc: func(ctx context.Context) ([]fs.Bucket, error) {
			return []fs.Bucket{}, nil
		},
	}
}

func NewClient(t testing.TB, srv *TestServer) *minio.Client {
	t.Helper()

	client, err := minio.New(srv.Endpoint, &minio.Options{})
	require.NoError(t, err)

	return client
}

type TestServer struct {
	Endpoint string
}

func newTestServer(t testing.TB, svc fs.Service) *TestServer {
	t.Helper()

	srv := httptest.NewServer(handler.New(svc))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	return &TestServer{
		Endpoint: u.Host,
	}
}

func newTestClient(t testing.TB, svc fs.Service) *minio.Client {
	t.Helper()

	srv := newTestServer(t, svc)

	return NewClient(t, srv)
}

// newTestHandler creates an HTTP handler for direct testing without starting a server.
func newTestHandler(svc fs.Service) http.Handler {
	return handler.New(svc)
}

// newTestRequest creates an HTTP request for testing.
//
//nolint:unparam
func newTestRequest(t testing.TB, method, path string) *http.Request {
	t.Helper()

	req := httptest.NewRequest(method, path, http.NoBody)
	req = req.WithContext(context.Background())

	return req
}

// newTestResponseRecorder creates a response recorder for testing.
func newTestResponseRecorder() *httptest.ResponseRecorder {
	return httptest.NewRecorder()
}
