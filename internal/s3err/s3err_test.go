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
		{fs.ErrInvalidBucketName, "InvalidBucketName"},
		{fs.ErrPreconditionFailed, "PreconditionFailed"},
		{fs.ErrUnsupportedOperation, "NotImplemented"},
		{errors.Wrap(fs.ErrObjectNotFound, "wrapped"), "NoSuchKey"},
		{errors.New("something else"), "InternalError"},
		{nil, "InternalError"},
	}

	for _, c := range cases {
		require.Equal(t, c.code, s3err.FromError(c.err).Code)
	}
}

func TestWrite_XMLBody(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("x-amz-request-id", "REQ123")

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", http.NoBody)

	s3err.Write(rec, req, fs.ErrObjectNotFound)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "application/xml", rec.Header().Get("Content-Type"))

	var body struct {
		XMLName   xml.Name `xml:"Error"`
		Code      string   `xml:"Code"`
		Message   string   `xml:"Message"`
		Resource  string   `xml:"Resource"`
		RequestID string   `xml:"RequestId"`
	}
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "NoSuchKey", body.Code)
	require.NotEmpty(t, body.Message)
	require.Equal(t, "/bucket/key", body.Resource)
	require.Equal(t, "REQ123", body.RequestID)
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
