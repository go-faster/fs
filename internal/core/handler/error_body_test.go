package handler_test

import (
	"encoding/xml"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// errorBody is an S3 error document with every element a test asserts on.
type errorBody struct {
	Code             string  `xml:"Code"`
	Resource         *string `xml:"Resource"`
	BucketName       string  `xml:"BucketName"`
	Key              string  `xml:"Key"`
	Condition        string  `xml:"Condition"`
	RangeRequested   string  `xml:"RangeRequested"`
	ActualObjectSize *int64  `xml:"ActualObjectSize"`
	ArgumentName     string  `xml:"ArgumentName"`
	ArgumentValue    string  `xml:"ArgumentValue"`
}

func parseErrorBody(t *testing.T, body []byte) errorBody {
	t.Helper()

	var e errorBody
	require.NoError(t, xml.Unmarshal(body, &e))
	require.Nil(t, e.Resource, "AWS S3 puts no Resource in an error body")

	return e
}

// TestErrorBody_Details covers the elements handlers add that the request
// alone does not tell, matching what AWS S3 sends for each.
func TestErrorBody_Details(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket/obj", "hello", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket/empty", "", nil).Code)

	t.Run("InvalidRange", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket/obj", "", map[string]string{"Range": "bytes=100-200"})
		require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)

		e := parseErrorBody(t, rec.Body.Bytes())
		require.Equal(t, "InvalidRange", e.Code)
		require.Equal(t, "bytes=100-200", e.RangeRequested)
		require.Equal(t, int64(5), *e.ActualObjectSize)
	})
	t.Run("InvalidRangeEmpty", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket/empty", "", map[string]string{"Range": "bytes=0-1"})

		e := parseErrorBody(t, rec.Body.Bytes())
		require.Equal(t, "InvalidRange", e.Code)
		require.Equal(t, int64(0), *e.ActualObjectSize)
	})
	t.Run("IfMatch", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket/obj", "", map[string]string{"If-Match": `"nope"`})
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)

		e := parseErrorBody(t, rec.Body.Bytes())
		require.Equal(t, "If-Match", e.Condition)
	})
	t.Run("CopySourceMissing", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/bucket/dst", "", map[string]string{"X-Amz-Copy-Source": "/bucket/missing"})
		require.Equal(t, http.StatusNotFound, rec.Code)

		e := parseErrorBody(t, rec.Body.Bytes())
		require.Equal(t, "NoSuchKey", e.Code)
		require.Equal(t, "missing", e.Key, "the source key, not the destination")
	})
	t.Run("CopySourceBucketMissing", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/bucket/dst", "", map[string]string{"X-Amz-Copy-Source": "/nowhere/k"})

		e := parseErrorBody(t, rec.Body.Bytes())
		require.Equal(t, "NoSuchBucket", e.Code)
		require.Equal(t, "nowhere", e.BucketName)
	})
	t.Run("MaxKeys", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/bucket?list-type=2&max-keys=-1", "", nil)

		e := parseErrorBody(t, rec.Body.Bytes())
		require.Equal(t, "InvalidArgument", e.Code)
		require.Equal(t, "maxKeys", e.ArgumentName)
		require.Equal(t, "-1", e.ArgumentValue)
	})
	t.Run("NoSuchBucket", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/nowhere/k", "", nil)

		e := parseErrorBody(t, rec.Body.Bytes())
		require.Equal(t, "NoSuchBucket", e.Code)
		require.Equal(t, "nowhere", e.BucketName)
		require.Empty(t, e.Key)
	})
}
