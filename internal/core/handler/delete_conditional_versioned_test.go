package handler_test

import (
	"encoding/xml"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/storagefs"
)

// versionedHandler serves a versioned bucket over storagefs, which is the only
// backend that versions. The in-memory backend used by the rest of these tests
// is not an fs.Versioner, so it cannot reach the path under test at all.
func versionedHandler(t testing.TB) http.Handler {
	t.Helper()

	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	h := handler.New(service.New(storage))

	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/versioned", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/versioned?versioning",
		`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, nil).Code)

	return h
}

// TestConditionalDeleteOnVersionedBucket is the end-to-end guard on #233.
//
// It has to run through the handler, not the backend: the defect was in
// routing, not in storage. A conditional delete took the unversioned path,
// looked for the object in the plain key tree, did not find it there (it lives
// in the version tree), and the miss was answered 204 — the same answer a
// correct conditional delete of an already-absent key gives. The condition was
// never evaluated, and nothing said so.
func TestConditionalDeleteOnVersionedBucket(t *testing.T) {
	t.Parallel()

	h := versionedHandler(t)

	put := do(t, h, http.MethodPut, "/versioned/k", "hello", nil)
	require.Equal(t, http.StatusOK, put.Code)

	etag := put.Header().Get("ETag")
	require.NotEmpty(t, etag)

	t.Run("MismatchRefuses", func(t *testing.T) {
		rec := do(t, h, http.MethodDelete, "/versioned/k", "", map[string]string{"If-Match": `"deadbeef"`})

		require.Equal(t, http.StatusPreconditionFailed, rec.Code,
			"a conditional delete on a versioned bucket must evaluate its condition")
		require.Equal(t, "PreconditionFailed", errorCode(t, rec.Body.String()))

		// The object survives: a refused delete must not have written a
		// tombstone on its way to refusing.
		require.Equal(t, http.StatusOK, do(t, h, http.MethodHead, "/versioned/k", "", nil).Code)
	})

	t.Run("MatchDeletes", func(t *testing.T) {
		rec := do(t, h, http.MethodDelete, "/versioned/k", "", map[string]string{"If-Match": etag})

		require.Equal(t, http.StatusNoContent, rec.Code)
		require.Equal(t, "true", rec.Header().Get("x-amz-delete-marker"),
			"a delete on a versioned bucket leaves a marker")
		require.Equal(t, http.StatusNotFound, do(t, h, http.MethodHead, "/versioned/k", "", nil).Code)
	})
}

// TestConditionalDeleteObjectsOnVersionedBucket covers the batch path, which
// had the same defect and reports it per object rather than per request: a
// failed condition must come back as an <Error>, not as a <Deleted>.
func TestConditionalDeleteObjectsOnVersionedBucket(t *testing.T) {
	t.Parallel()

	h := versionedHandler(t)

	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/versioned/k", "hello", nil).Code)

	body := `<Delete><Object><Key>k</Key><ETag>"deadbeef"</ETag></Object></Delete>`
	rec := do(t, h, http.MethodPost, "/versioned?delete", body, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var result struct {
		Deleted []struct {
			Key string `xml:"Key"`
		} `xml:"Deleted"`
		Errors []struct {
			Key  string `xml:"Key"`
			Code string `xml:"Code"`
		} `xml:"Error"`
	}

	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))

	require.Empty(t, result.Deleted, "a failed condition must not report the object deleted")
	require.Len(t, result.Errors, 1)
	require.Equal(t, "k", result.Errors[0].Key)
	require.Equal(t, "PreconditionFailed", result.Errors[0].Code)

	// And the object is still there.
	require.Equal(t, http.StatusOK, do(t, h, http.MethodHead, "/versioned/k", "", nil).Code)
}
