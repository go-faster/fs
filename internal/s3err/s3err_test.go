package s3err_test

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

func TestFromError(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{fs.ErrBucketNotFound, "NoSuchBucket"},
		{fs.ErrObjectNotFound, "NoSuchKey"},
		{fs.ErrUploadNotFound, "NoSuchUpload"},
		{fs.ErrBucketAlreadyExists, "BucketAlreadyOwnedByYou"},
		{fs.ErrBucketNotEmpty, "BucketNotEmpty"},
		{fs.ErrQuotaExceeded, "QuotaExceeded"},
		{fs.ErrInvalidBucketName, "InvalidBucketName"},
		{fs.ErrPreconditionFailed, "PreconditionFailed"},
		{fs.ErrUnsupportedOperation, "NotImplemented"},
		{fs.ErrUnavailable, "ServiceUnavailable"},
		{errors.Wrap(fs.ErrObjectNotFound, "wrapped"), "NoSuchKey"},
		{errors.New("something else"), "InternalError"},
		{nil, "InternalError"},
	}

	for _, c := range cases {
		require.Equal(t, c.code, s3err.FromError(c.err).Code)
	}
}

// TestWrite_XMLBody pins the body to the one AWS S3 sends for a missing key
// (probed 2026-10-26): the key, and no <Resource>.
func TestWrite_XMLBody(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("x-amz-request-id", "REQ123")

	req := httptest.NewRequest(http.MethodGet, "/bucket/no/such%20key", http.NoBody)

	s3err.Write(rec, req, fs.ErrObjectNotFound)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "application/xml", rec.Header().Get("Content-Type"))
	require.Equal(t, xml.Header+`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>`+
		`<Key>no/such key</Key><RequestId>REQ123</RequestId></Error>`, rec.Body.String())
}

// TestWrite_Detail covers the elements each code carries, as AWS S3 renders
// them.
func TestWrite_Detail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		target string
		header map[string]string
		err    error
		want   string
	}{
		{
			name: "NoSuchBucket", method: http.MethodGet, target: "/missing/k", err: fs.ErrBucketNotFound,
			want: `<Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message><BucketName>missing</BucketName>`,
		},
		{
			name: "BucketNotEmpty", method: http.MethodDelete, target: "/full", err: fs.ErrBucketNotEmpty,
			want: `<Code>BucketNotEmpty</Code><Message>The bucket you tried to delete is not empty.</Message><BucketName>full</BucketName>`,
		},
		{
			name: "NoSuchUpload", method: http.MethodGet, target: "/b/k?uploadId=bogus", err: fs.ErrUploadNotFound,
			want: `<Code>NoSuchUpload</Code><Message>The specified upload does not exist. The upload ID may be invalid, ` +
				`or the upload may have been aborted or completed.</Message><UploadId>bogus</UploadId>`,
		},
		{
			name: "DeleteMarker", method: http.MethodGet, target: "/b/k?versionId=v", err: fs.ErrMethodNotAllowedOnDeleteMarker,
			want: `<Code>MethodNotAllowed</Code><Message>The specified method is not allowed against this resource.</Message>` +
				`<Method>GET</Method><ResourceType>DeleteMarker</ResourceType>`,
		},
		{
			name: "PreconditionFailed", method: http.MethodPut, target: "/b/k", header: map[string]string{"If-None-Match": "*"},
			err: fs.ErrPreconditionFailed,
			want: `<Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message>` +
				`<Condition>If-None-Match</Condition>`,
		},
		{
			name: "ExplicitWins", method: http.MethodPut, target: "/dst/k",
			err:  s3err.WithDetail(fs.ErrObjectNotFound, s3err.Detail{Key: "source/key"}),
			want: `<Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>source/key</Key>`,
		},
		{
			name: "AccessDenied", method: http.MethodGet, target: "/b/k", err: fs.ErrAccessDenied,
			want: `<Code>AccessDenied</Code><Message>Access Denied</Message>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.target, http.NoBody)

			for k, v := range tc.header {
				req.Header.Set(k, v)
			}

			s3err.Write(rec, req, tc.err)
			require.Equal(t, xml.Header+"<Error>"+tc.want+"<RequestId></RequestId></Error>", rec.Body.String())
		})
	}
}

// TestWriteAPI_Detail renders an explicit Detail, here InvalidRange's.
func TestWriteAPI_Detail(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/b/k", http.NoBody)

	s3err.WriteAPI(rec, req, s3err.InvalidRange, s3err.Detail{RangeRequested: "bytes=100-200", ActualObjectSize: s3err.Size(5)})
	require.Equal(t, xml.Header+`<Error><Code>InvalidRange</Code><Message>The requested range is not satisfiable</Message>`+
		`<RangeRequested>bytes=100-200</RangeRequested><ActualObjectSize>5</ActualObjectSize><RequestId></RequestId></Error>`,
		rec.Body.String())
}

func TestWrite_HeadHasNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/bucket/key", http.NoBody)

	s3err.Write(rec, req, fs.ErrObjectNotFound)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Empty(t, rec.Body.Bytes())
	require.Empty(t, rec.Header().Get("Content-Type"))
}

func TestWriteAPI_ExplicitCode(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/bucket?delete", http.NoBody)

	s3err.WriteAPI(rec, req, s3err.MalformedXML)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "<Code>MalformedXML</Code>")
}
