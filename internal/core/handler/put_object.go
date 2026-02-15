package handler

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-faster/fs"
)

// getBodyReader returns the appropriate reader for the request body,
// handling AWS chunked encoding if necessary.
func getBodyReader(r *http.Request) io.Reader {
	var reader io.Reader = r.Body
	contentEncoding := r.Header.Get("Content-Encoding")
	contentSha256 := r.Header.Get("X-Amz-Content-Sha256")
	if isAWSChunkedEncoding(contentEncoding) || isAWSStreamingPayload(contentSha256) {
		reader = newAWSChunkedReader(r.Body)
	}
	return reader
}

// getDecodedContentLength returns the decoded content length for AWS chunked uploads.
func getDecodedContentLength(r *http.Request) int64 {
	size := r.ContentLength
	if decodedLength := r.Header.Get("X-Amz-Decoded-Content-Length"); decodedLength != "" {
		if parsed, err := parseInt64(decodedLength); err == nil {
			size = parsed
		}
	}
	return size
}

func (h *handler) PutObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	// Check if this is an upload part request
	query := r.URL.Query()
	if query.Get("uploadId") != "" && query.Get("partNumber") != "" {
		h.UploadPart(w, r)
		return
	}

	// Handle AWS chunked encoding
	reader := getBodyReader(r)
	size := getDecodedContentLength(r)

	req := &fs.PutObjectRequest{
		Reader: reader,
		Bucket: bucket,
		Key:    key,
		Size:   size,
	}

	if err := h.service.PutObject(ctx, req); err != nil {
		renderError(ctx, w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, nil
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}
