package handler_test

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/root/handler"
)

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
