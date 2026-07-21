package handler

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
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

	// Check if this is an upload part request (with x-amz-copy-source it is an
	// UploadPartCopy).
	query := r.URL.Query()
	if query.Get("uploadId") != "" && query.Get("partNumber") != "" {
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			h.UploadPartCopy(w, r)
			return
		}

		h.UploadPart(w, r)

		return
	}

	// Server-side copy is signaled by the x-amz-copy-source header.
	if r.Header.Get("X-Amz-Copy-Source") != "" {
		h.CopyObject(w, r)
		return
	}

	tags, err := parseTaggingHeader(r.Header.Get("X-Amz-Tagging"))
	if err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
		return
	}

	// Handle AWS chunked encoding.
	reader := getBodyReader(r)
	size := getDecodedContentLength(r)

	// If-Match / If-None-Match are forwarded to the storage layer, which
	// evaluates them atomically with the write so concurrent conditional PUTs
	// resolve to a single winner. On failure it returns ErrPreconditionFailed,
	// which maps to 412.
	req := &fs.PutObjectRequest{
		Reader:      reader,
		Bucket:      bucket,
		Key:         key,
		Size:        size,
		Metadata:    extractObjectMetadata(r.Header),
		Tags:        tags,
		ACL:         fs.ParseACL(r.Header.Get("X-Amz-Acl")),
		IfNoneMatch: r.Header.Get("If-None-Match"),
		IfMatch:     r.Header.Get("If-Match"),
	}

	resp, err := h.service.PutObject(ctx, req)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.Header().Set("ETag", quoteETag(resp.ETag))
	w.WriteHeader(http.StatusOK)
}
