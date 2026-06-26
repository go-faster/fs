package handler_test

import (
	"encoding/xml"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/handler"
)

func TestCopyObject(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/src", "payload", nil).Code)

	rec := do(t, h, http.MethodPut, "/bucket-a/dst", "", map[string]string{
		"X-Amz-Copy-Source": "/bucket-a/src",
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var result handler.CopyObjectResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))
	require.NotEmpty(t, result.ETag)

	// Destination has the copied bytes.
	require.Equal(t, "payload", do(t, h, http.MethodGet, "/bucket-a/dst", "", nil).Body.String())

	// Missing source -> 404.
	require.Equal(t, http.StatusNotFound, do(t, h, http.MethodPut, "/bucket-a/dst2", "", map[string]string{
		"X-Amz-Copy-Source": "/bucket-a/absent",
	}).Code)
}
