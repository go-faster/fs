package handler_test

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVersionID_Errors pins the answers AWS S3 gives to version IDs
// (probed 2026-10-26): a malformed one is InvalidArgument naming the
// argument, on every object request; a well-formed one that is not there is
// NoSuchVersion naming key and version, even when the key is missing.
func TestVersionID_Errors(t *testing.T) {
	h := lifecycleEngineHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket?versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket/obj", "hello", nil).Code)

	missing := strings.Repeat("ab", 16)

	type body struct {
		Code          string `xml:"Code"`
		Key           string `xml:"Key"`
		VersionID     string `xml:"VersionId"`
		ArgumentName  string `xml:"ArgumentName"`
		ArgumentValue string `xml:"ArgumentValue"`
	}

	for _, tc := range []struct {
		name, method, target string
		header               map[string]string
		status               int
		want                 body
	}{
		{
			name: "GetMalformed", method: http.MethodGet, target: "/bucket/obj?versionId=bogus", status: http.StatusBadRequest,
			want: body{Code: "InvalidArgument", ArgumentName: "versionId", ArgumentValue: "bogus"},
		},
		{
			name: "GetEmpty", method: http.MethodGet, target: "/bucket/obj?versionId=", status: http.StatusBadRequest,
			want: body{Code: "InvalidArgument", ArgumentName: "versionId"},
		},
		{
			name: "DeleteMalformed", method: http.MethodDelete, target: "/bucket/obj?versionId=bogus", status: http.StatusBadRequest,
			want: body{Code: "InvalidArgument", ArgumentName: "versionId", ArgumentValue: "bogus"},
		},
		{
			name: "TaggingMalformed", method: http.MethodGet, target: "/bucket/obj?tagging&versionId=bogus", status: http.StatusBadRequest,
			want: body{Code: "InvalidArgument", ArgumentName: "versionId", ArgumentValue: "bogus"},
		},
		{
			name: "GetMissingVersion", method: http.MethodGet, target: "/bucket/obj?versionId=" + missing, status: http.StatusNotFound,
			want: body{Code: "NoSuchVersion", Key: "obj", VersionID: missing},
		},
		{
			name: "NullOnMissingKey", method: http.MethodGet, target: "/bucket/missing?versionId=null", status: http.StatusNotFound,
			want: body{Code: "NoSuchVersion", Key: "missing", VersionID: "null"},
		},
		{
			name: "CopySourceMalformed", method: http.MethodPut, target: "/bucket/dst", status: http.StatusBadRequest,
			header: map[string]string{"X-Amz-Copy-Source": "/bucket/obj?versionId=bogus"},
			want:   body{Code: "InvalidRequest"},
		},
		{
			name: "CopySourceMissingVersion", method: http.MethodPut, target: "/bucket/dst", status: http.StatusNotFound,
			header: map[string]string{"X-Amz-Copy-Source": "/bucket/obj?versionId=" + missing},
			want:   body{Code: "NoSuchVersion", Key: "obj", VersionID: missing},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.target, "", tc.header)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())

			var got body
			require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, tc.want, got)
		})
	}

	t.Run("HeadMalformed", func(t *testing.T) {
		rec := do(t, h, http.MethodHead, "/bucket/obj?versionId=bogus", "", nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Empty(t, rec.Body.Bytes())
	})
	t.Run("HeadMissingVersion", func(t *testing.T) {
		rec := do(t, h, http.MethodHead, "/bucket/obj?versionId="+missing, "", nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("DeleteMissingVersion", func(t *testing.T) {
		// Deleting a well-formed version that is not there succeeds, as on AWS.
		rec := do(t, h, http.MethodDelete, "/bucket/obj?versionId="+missing, "", nil)
		require.Equal(t, http.StatusNoContent, rec.Code)
	})
}
