package handler

import (
	"net/http"
	"strconv"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// servePartNumber answers a GET or HEAD carrying ?partNumber=N by serving that
// part's byte range, and reports whether it handled the request.
//
// S3 lets a client read back a multipart object one part at a time, which is
// how a downloader parallelizes without knowing the layout in advance: the
// response carries x-amz-mp-parts-count so the client learns how many more
// there are. A single PUT has exactly one part, so partNumber=1 returns the
// whole object and anything else is InvalidPart.
func (h *handler) servePartNumber(w http.ResponseWriter, r *http.Request, bucket, key, raw string) bool {
	ctx := r.Context()

	part, err := strconv.Atoi(raw)
	if err != nil || part < 1 {
		renderAPIError(ctx, w, r, s3err.InvalidArgument,
			errors.Errorf("invalid partNumber %q", raw))

		return true
	}

	attributer, ok := h.service.(fs.ObjectAttributer)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return true
	}

	attrs, err := attributer.ObjectAttributes(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, r, err)
		return true
	}

	offset, length, ok := attrs.PartRange(part)
	if !ok {
		renderAPIError(ctx, w, r, s3err.InvalidPart,
			errors.Errorf("object has %d parts, requested %d", attrs.PartsCount(), part))

		return true
	}

	resp, err := h.service.GetObject(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, r, err)
		return true
	}

	// The part list is a property of the whole object, so the response reports
	// the object's ETag and the part count, not the part's own identity — the
	// same thing S3 does.
	if count := attrs.PartsCount(); count > 0 {
		w.Header().Set("x-amz-mp-parts-count", strconv.Itoa(count))
	}

	// Reuse the range machinery: the part is simply the byte range it occupies.
	// Its status is 200, not 206 — the client asked for a part, not a range, and
	// got the whole of what it asked for.
	r = r.Clone(ctx)
	r.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+length-1, 10))

	serveObject(&partResponseWriter{ResponseWriter: w}, r, key, resp)

	return true
}

// partResponseWriter rewrites the 206 that serving a byte range produces into
// the 200 a ?partNumber read reports, and drops the Content-Range that goes
// with it.
type partResponseWriter struct {
	http.ResponseWriter
	written bool
}

func (w *partResponseWriter) WriteHeader(status int) {
	if w.written {
		return
	}

	w.written = true

	if status == http.StatusPartialContent {
		w.Header().Del("Content-Range")

		status = http.StatusOK
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *partResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}

	return w.ResponseWriter.Write(b)
}
