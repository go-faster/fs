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
		// An SDK reads the error code out of the XML body; ServeContent's own
		// text/plain body would leave it with nothing to parse.
		require.Contains(t, rec.Body.String(), "<Code>InvalidRange</Code>")
		require.Equal(t, "application/xml", rec.Header().Get("Content-Type"))
	})

	t.Run("Full", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-a/obj", "", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "0123456789", rec.Body.String())
		require.NotEmpty(t, rec.Header().Get("ETag"))
	})
}

// Any range against a zero-length object is unsatisfiable in S3. ServeContent
// ignores the header instead and would answer 200.
func TestGetObject_RangeOnEmptyObject(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-e", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-e/empty", "", nil).Code)

	for _, rng := range []string{"bytes=40-50", "bytes=0-0", "bytes=0-"} {
		t.Run(rng, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, "/bucket-e/empty", "", map[string]string{"Range": rng})
			require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
			require.Contains(t, rec.Body.String(), "<Code>InvalidRange</Code>")
		})
	}

	t.Run("NoRangeServesEmptyBody", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket-e/empty", "", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Empty(t, rec.Body.String())
	})
}

// A failed precondition must carry the PreconditionFailed code, not the bare
// status ServeContent's text/plain body leaves an SDK to fall back on.
func TestGetObject_PreconditionFailedCarriesS3Code(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-p", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-p/obj", "body", nil).Code)

	rec := do(t, h, http.MethodGet, "/bucket-p/obj", "", map[string]string{
		"If-Unmodified-Since": "Sat, 29 Oct 1994 19:43:31 GMT",
	})
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	require.Contains(t, rec.Body.String(), "<Code>PreconditionFailed</Code>")
}
