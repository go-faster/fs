package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetObject_Conditional(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/obj", "hello", nil).Code)

	etag := do(t, h, http.MethodGet, "/bucket-a/obj", "", nil).Header().Get("ETag")
	require.NotEmpty(t, etag)

	t.Run("IfNoneMatch_NotModified", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", map[string]string{"If-None-Match": etag})
		require.Equal(t, http.StatusNotModified, rec.Code)
	})

	t.Run("IfMatch_Ok", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", map[string]string{"If-Match": etag})
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("IfMatch_Mismatch", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", map[string]string{"If-Match": `"deadbeef"`})
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	})
}
