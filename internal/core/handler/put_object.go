package handler

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/fs"
)

// getBodyReader returns the appropriate reader for the request body,
// handling AWS chunked encoding if necessary.
func getBodyReader(r *http.Request) io.Reader {
	var reader io.Reader = r.Body

	contentEncoding := r.Header.Get("Content-Encoding")

	contentSHA256 := r.Header.Get("X-Amz-Content-Sha256")
	if isAWSChunkedEncoding(contentEncoding) || isAWSStreamingPayload(contentSHA256) {
		reader = newAWSChunkedReader(r.Body)
	}

	return reader
}

// getDecodedContentLength returns the decoded content length for AWS chunked uploads.
func getDecodedContentLength(r *http.Request) int64 {
	size := r.ContentLength
	if decodedLength := r.Header.Get("X-Amz-Decoded-Content-Length"); decodedLength != "" {
		if parsed, err := strconv.ParseInt(decodedLength, 10, 64); err == nil {
			size = parsed
		}
	}

	return size
}

func (h *handler) PutObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	// Check if this is an upload part request.
	query := r.URL.Query()
	if query.Get("uploadId") != "" && query.Get("partNumber") != "" {
		h.UploadPart(w, r)
		return
	}

	// Handle AWS chunked encoding.
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
