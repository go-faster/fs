package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/cors"
	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/storagemem"
)

func newCORSHandler(t testing.TB, cfg cors.Config) http.Handler {
	t.Helper()

	return handler.New(service.New(storagemem.New()), handler.WithCORS(cfg))
}

func corsConfig() cors.Config {
	return cors.Config{
		Default: []cors.Rule{{
			AllowedOrigins: []string{"https://app.example.com"},
			AllowedMethods: []string{http.MethodGet, http.MethodPut},
			AllowedHeaders: []string{"x-amz-acl", "content-type"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  600,
		}},
	}
}

func TestCORS_Preflight(t *testing.T) {
	h := newCORSHandler(t, corsConfig())

	t.Run("Allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/bucket/obj", http.NoBody)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		req.Header.Set("Access-Control-Request-Headers", "content-type")

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
		require.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "PUT")
		require.Equal(t, "content-type", rec.Header().Get("Access-Control-Allow-Headers"))
		require.Equal(t, "600", rec.Header().Get("Access-Control-Max-Age"))
	})

	t.Run("DisallowedOrigin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/bucket/obj", http.NoBody)
		req.Header.Set("Origin", "https://evil.example.com")
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusForbidden, rec.Code)
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("DisallowedMethod", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/bucket/obj", http.NoBody)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", http.MethodDelete)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("DisallowedHeader", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/bucket/obj", http.NoBody)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		req.Header.Set("Access-Control-Request-Headers", "x-secret-header")

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		require.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestCORS_ActualRequest(t *testing.T) {
	h := newCORSHandler(t, corsConfig())
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	t.Run("CrossOriginGetsHeaders", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a", "", map[string]string{"Origin": "https://app.example.com"})
		require.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
		require.Equal(t, "ETag", rec.Header().Get("Access-Control-Expose-Headers"))
		require.Contains(t, rec.Header().Values("Vary"), "Origin")
	})

	t.Run("NoOriginNoHeaders", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a", "", nil)
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("UnknownOriginNoHeaders", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a", "", map[string]string{"Origin": "https://other.example.com"})
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})
}
