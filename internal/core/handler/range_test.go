package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetObject_Range(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/obj", "0123456789", nil).Code)

	t.Run("Closed", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", map[string]string{"Range": "bytes=2-5"})
		require.Equal(t, http.StatusPartialContent, rec.Code)
		require.Equal(t, "2345", rec.Body.String())
		require.Equal(t, "bytes 2-5/10", rec.Header().Get("Content-Range"))
	})

	t.Run("OpenEnded", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", map[string]string{"Range": "bytes=7-"})
		require.Equal(t, http.StatusPartialContent, rec.Code)
		require.Equal(t, "789", rec.Body.String())
	})

	t.Run("Suffix", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", map[string]string{"Range": "bytes=-3"})
		require.Equal(t, http.StatusPartialContent, rec.Code)
		require.Equal(t, "789", rec.Body.String())
	})

	t.Run("Unsatisfiable", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", map[string]string{"Range": "bytes=100-200"})
		require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
	})

	t.Run("Full", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "0123456789", rec.Body.String())
		require.NotEmpty(t, rec.Header().Get("ETag"))
	})
}
