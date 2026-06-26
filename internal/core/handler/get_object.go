package handler

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (h *handler) GetObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	resp, err := h.service.GetObject(ctx, bucket, key)
	if errors.Is(err, fs.ErrObjectNotFound) || errors.Is(err, fs.ErrBucketNotFound) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err != nil {
		renderError(ctx, w, err)
		return
	}

	serveObject(w, r, key, resp)
}

// quoteETag returns the ETag as a quoted string, as required by S3/HTTP.
func quoteETag(etag string) string {
	if etag == "" || strings.HasPrefix(etag, `"`) {
		return etag
	}

	return `"` + etag + `"`
}

// serveObject writes an object response, delegating to http.ServeContent when the
// reader is seekable so that Range requests (206 + Content-Range) and conditional
// headers (If-Range, If-Modified-Since, If-Match, If-None-Match) are handled. It is
// safe for HEAD requests. The reader is always closed.
func serveObject(w http.ResponseWriter, r *http.Request, key string, resp *fs.GetObjectResponse) {
	defer func() { _ = resp.Reader.Close() }()

	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	if resp.ETag != "" {
		w.Header().Set("ETag", quoteETag(resp.ETag))
	}

	if rs, ok := resp.Reader.(io.ReadSeeker); ok {
		// ServeContent handles Range, conditional requests, Content-Range,
		// Last-Modified and the 206/304/412/416 status codes, and writes no body
		// for HEAD requests.
		http.ServeContent(w, r, key, resp.LastModified, rs)
		return
	}

	// Fallback for non-seekable readers: full body, no range support.
	if resp.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.Size, 10))
	}

	if !resp.LastModified.IsZero() {
		w.Header().Set("Last-Modified", resp.LastModified.UTC().Format(http.TimeFormat))
	}

	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Reader)
	}
}
