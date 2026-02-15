package handler

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-faster/fs"
)

func (h *handler) PutObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	// Handle AWS chunked encoding
	var reader io.Reader = r.Body
	contentEncoding := r.Header.Get("Content-Encoding")
	contentSha256 := r.Header.Get("X-Amz-Content-Sha256")
	if isAWSChunkedEncoding(contentEncoding) || isAWSStreamingPayload(contentSha256) {
		reader = newAWSChunkedReader(r.Body)
	}

	// Get decoded content length if available
	size := r.ContentLength
	if decodedLength := r.Header.Get("X-Amz-Decoded-Content-Length"); decodedLength != "" {
		// Use the decoded content length for AWS chunked uploads
		if parsed, err := parseInt64(decodedLength); err == nil {
			size = parsed
		}
	}

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
