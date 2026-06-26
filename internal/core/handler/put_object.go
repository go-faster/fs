package handler

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

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

	// Evaluate If-Match / If-None-Match preconditions; on failure respond with
	// 412 Precondition Failed.
	if failed, err := h.checkPutPreconditions(ctx, bucket, key, r.Header); err != nil {
		renderError(ctx, w, err)
		return
	} else if failed {
		renderError(ctx, w, fs.ErrPreconditionFailed)
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

// statObject reports whether an object currently exists and its (unquoted) ETag.
// A missing bucket or object is reported as not existing rather than an error.
func (h *handler) statObject(ctx context.Context, bucket, key string) (exists bool, etag string, err error) {
	resp, err := h.service.GetObject(ctx, bucket, key)
	if errors.Is(err, fs.ErrObjectNotFound) || errors.Is(err, fs.ErrBucketNotFound) {
		return false, "", nil
	}

	if err != nil {
		return false, "", err
	}

	_ = resp.Reader.Close()

	return true, resp.ETag, nil
}

// checkPutPreconditions evaluates the If-Match / If-None-Match request headers
// against the current state of the target object. It returns true when the
// precondition fails (the caller should respond with 412).
//
//   - If-None-Match: *          fail if the object exists.
//   - If-None-Match: "<etag>"   fail if it exists and the ETag matches.
//   - If-Match: *               fail if the object does not exist.
//   - If-Match: "<etag>"        fail if it is missing or the ETag differs.
func (h *handler) checkPutPreconditions(ctx context.Context, bucket, key string, header http.Header) (bool, error) {
	ifNoneMatch := strings.TrimSpace(header.Get("If-None-Match"))
	ifMatch := strings.TrimSpace(header.Get("If-Match"))

	if ifNoneMatch == "" && ifMatch == "" {
		return false, nil
	}

	exists, etag, err := h.statObject(ctx, bucket, key)
	if err != nil {
		return false, err
	}

	if ifNoneMatch == "*" && exists {
		return true, nil
	}

	if ifNoneMatch != "" && ifNoneMatch != "*" && exists && etagMatches(ifNoneMatch, etag) {
		return true, nil
	}

	if ifMatch == "*" && !exists {
		return true, nil
	}

	if ifMatch != "" && ifMatch != "*" && (!exists || !etagMatches(ifMatch, etag)) {
		return true, nil
	}

	return false, nil
}

// etagMatches reports whether the raw ETag matches any entity-tag in an If-Match
// or If-None-Match header value (a comma-separated list, each optionally quoted
// or weak-prefixed with W/).
func etagMatches(header, raw string) bool {
	raw = strings.Trim(raw, `"`)

	for tok := range strings.SplitSeq(header, ",") {
		tok = strings.TrimSpace(tok)
		tok = strings.TrimPrefix(tok, "W/")

		if strings.Trim(tok, `"`) == raw {
			return true
		}
	}

	return false
}
