package handler_test

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/handler"
)

// TestDeleteObject_Conditional covers If-Match on DeleteObject: a mismatch
// against a live object is 412, a match deletes, and any condition against a
// key that is already gone still reports the idempotent 204.
func TestDeleteObject_Conditional(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/obj", "body", nil).Code)

	etag := do(t, h, http.MethodGet, "/bucket-a/obj", "", nil).Header().Get("ETag")

	require.Equal(t, http.StatusPreconditionFailed,
		do(t, h, http.MethodDelete, "/bucket-a/obj", "", map[string]string{"If-Match": `"nope"`}).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodGet, "/bucket-a/obj", "", nil).Code)

	require.Equal(t, http.StatusNoContent,
		do(t, h, http.MethodDelete, "/bucket-a/obj", "", map[string]string{"If-Match": etag}).Code)

	// Gone: a condition has nothing to guard, so the delete still succeeds.
	require.Equal(t, http.StatusNoContent,
		do(t, h, http.MethodDelete, "/bucket-a/obj", "", map[string]string{"If-Match": `"nope"`}).Code)
	require.Equal(t, http.StatusNoContent,
		do(t, h, http.MethodDelete, "/bucket-a/obj", "", map[string]string{"If-Match": "*"}).Code)
}

// TestDeleteObject_ConditionalSize covers the x-amz-if-match-size extension,
// including the malformed value that must not be silently ignored.
func TestDeleteObject_ConditionalSize(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/obj", "body", nil).Code)

	require.Equal(t, http.StatusPreconditionFailed,
		do(t, h, http.MethodDelete, "/bucket-a/obj", "", map[string]string{"x-amz-if-match-size": "9999"}).Code)

	// A size we cannot parse must fail the request, never widen it into an
	// unconditional delete.
	require.Equal(t, http.StatusBadRequest,
		do(t, h, http.MethodDelete, "/bucket-a/obj", "", map[string]string{"x-amz-if-match-size": "big"}).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodGet, "/bucket-a/obj", "", nil).Code)

	require.Equal(t, http.StatusNoContent,
		do(t, h, http.MethodDelete, "/bucket-a/obj", "", map[string]string{"x-amz-if-match-size": "4"}).Code)
}

// TestDeleteObjects_Conditional covers the per-key guards in a batch delete:
// a failed guard is reported per object, not as a request-level failure.
func TestDeleteObjects_Conditional(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/obj", "body", nil).Code)

	body := `<Delete><Object><Key>obj</Key><ETag>"nope"</ETag></Object></Delete>`
	rec := do(t, h, http.MethodPost, "/bucket-a?delete", body, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var result handler.DeleteObjectsResult
	require.NoError(t, xml.NewDecoder(strings.NewReader(rec.Body.String())).Decode(&result))
	require.Len(t, result.Errors, 1)
	require.Equal(t, "PreconditionFailed", result.Errors[0].Code)
	require.Empty(t, result.Deleted)

	// The object survived the failed guard.
	require.Equal(t, http.StatusOK, do(t, h, http.MethodGet, "/bucket-a/obj", "", nil).Code)

	etag := do(t, h, http.MethodGet, "/bucket-a/obj", "", nil).Header().Get("ETag")
	body = `<Delete><Object><Key>obj</Key><ETag>` + etag + `</ETag></Object></Delete>`
	rec = do(t, h, http.MethodPost, "/bucket-a?delete", body, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	result = handler.DeleteObjectsResult{}
	require.NoError(t, xml.NewDecoder(strings.NewReader(rec.Body.String())).Decode(&result))
	require.Empty(t, result.Errors)
	require.Len(t, result.Deleted, 1)
}
