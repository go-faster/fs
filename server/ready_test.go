package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

func TestServer_Readiness(t *testing.T) {
	t.Run("ReadyByDefault", func(t *testing.T) {
		srv, err := server.New(server.Config{Storage: storagemem.New()})
		require.NoError(t, err)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", http.NoBody))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), "READY")
	})

	t.Run("NotReady", func(t *testing.T) {
		srv, err := server.New(server.Config{
			Storage: storagemem.New(),
			Ready:   func(context.Context) error { return errors.New("storage unreachable") },
		})
		require.NoError(t, err)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", http.NoBody))

		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.Contains(t, rec.Body.String(), "storage unreachable")
	})

	t.Run("ReadyProbeReceivesRequestContext", func(t *testing.T) {
		var got context.Context

		srv, err := server.New(server.Config{
			Storage: storagemem.New(),
			Ready:   func(ctx context.Context) error { got = ctx; return nil },
		})
		require.NoError(t, err)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", http.NoBody))

		require.NotNil(t, got)
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("Disabled", func(t *testing.T) {
		srv, err := server.New(server.Config{Storage: storagemem.New(), ReadyPath: "-"})
		require.NoError(t, err)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", http.NoBody))

		// With readiness disabled, /ready falls through to the S3 router, which
		// treats it as a bucket request — not a 200 readiness response.
		require.NotEqual(t, "READY", rec.Body.String())
	})
}
