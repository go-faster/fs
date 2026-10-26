package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

// TestVirtualHostHealth: on a host that names a bucket, /health is a key in
// that bucket; on any other host it is the liveness check.
func TestVirtualHostHealth(t *testing.T) {
	storage := storagemem.New()
	require.NoError(t, storage.CreateBucket(t.Context(), "bkt"))

	srv, err := server.New(server.Config{Storage: storage, Domains: []string{"s3.test"}})
	require.NoError(t, err)

	get := func(host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
		req.Host = host

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := get("s3.test")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "OK", rec.Body.String())

	rec = get("10.0.0.1:8080")
	require.Equal(t, "OK", rec.Body.String())

	rec = get("bkt.s3.test")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "<Code>NoSuchKey</Code>")
}
