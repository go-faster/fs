package integration

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/server"
)

func NewClient(t testing.TB, srv *TestServer) *minio.Client {
	t.Helper()

	// Create client without credentials for local testing
	// Disable secure mode and use anonymous credentials to avoid signed chunks
	client, err := minio.New(srv.Endpoint, &minio.Options{
		Secure: false,
	})
	require.NoError(t, err)

	return client
}

type TestServer struct {
	// Endpoint is the host:port, used by minio-go.
	Endpoint string
	// URL is the full base URL (scheme://host:port), used by aws-sdk-go-v2.
	URL string
}

func newTestServer(t testing.TB, store fs.Storage) *TestServer {
	t.Helper()

	srv := httptest.NewServer(server.NewHandler(store))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	return &TestServer{
		Endpoint: u.Host,
		URL:      srv.URL,
	}
}
