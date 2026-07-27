package handler

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

func (h *handler) GetObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	if raw := r.URL.Query().Get("partNumber"); raw != "" {
		h.servePartNumber(w, r, bucket, key, raw)
		return
	}

	resp, err := h.service.GetObject(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, r, err)
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

// serveObject writes an object response through http.ServeContent, which handles
// Range requests (206 + Content-Range), conditional headers (If-Range,
// If-Modified-Since, If-Match, If-None-Match), Last-Modified, the
// 206/304/412/416 status codes and the empty body of a HEAD. The reader is
// always closed.
//
// A backend may hand back a reader that cannot seek — the cluster backend
// streams a replica from a peer over HTTP whenever the fragment it picks is not
// on the local disk. That is an implementation detail of where the bytes happen
// to live, so it must not change what the client sees: a streamed reader is
// wrapped in streamSeeker rather than served through a degraded path.
func serveObject(w http.ResponseWriter, r *http.Request, key string, resp *fs.GetObjectResponse) {
	defer func() { _ = resp.Reader.Close() }()

	// Stored representation headers and x-amz-meta-* pairs; without a stored
	// Content-Type the S3 default applies.
	w.Header().Set("Content-Type", "application/octet-stream")
	writeObjectMetadata(w.Header(), resp.Metadata)

	if resp.ETag != "" {
		w.Header().Set("ETag", quoteETag(resp.ETag))
	}

	w.Header().Set("Accept-Ranges", "bytes")

	writeSSE(w, resp.ServerSideEncryption)

	// S3 reports how many tags an object carries so a client can tell "no tags"
	// from "tags I have not fetched" without a second request.
	if resp.TagCount > 0 {
		w.Header().Set("x-amz-tagging-count", strconv.Itoa(resp.TagCount))
	}

	// response-* query parameters override the stored representation headers,
	// which is how a presigned URL controls what the browser does with the
	// object it points at.
	applyResponseOverrides(w.Header(), r.URL.Query())

	// A range against a zero-length object is unsatisfiable in S3. ServeContent
	// deliberately ignores it instead ("some clients add a Range header to
	// disable caching"), which would answer 200 where a client expects 416.
	if resp.Size == 0 && isByteRangeRequest(r) {
		renderAPIError(r.Context(), w, r, s3err.InvalidRange, errors.Errorf("range %q against an empty object", r.Header.Get("Range")))
		return
	}

	// ServeContent reports Range and conditional failures the net/http way — a
	// text/plain body — which carries no S3 error Code for an SDK to parse.
	w = &s3ErrorInterceptor{ResponseWriter: w, req: r}

	rs, seekable := resp.Reader.(io.ReadSeeker)
	if !seekable {
		// streamSeeker can only move forward, and ServeContent seeks back and
		// forth between the parts of a multi-range request. Drop the header:
		// serving the whole object for a multi-range GET is what S3 itself
		// does, since it supports only one range per request.
		if isMultiRange(r.Header.Get("Range")) {
			r = r.Clone(r.Context())
			r.Header.Del("Range")
		}

		rs = &streamSeeker{r: resp.Reader, size: resp.Size}
	}

	http.ServeContent(w, r, key, resp.LastModified, rs)
}

// isMultiRange reports whether a Range header names more than one range.
func isMultiRange(header string) bool {
	return strings.Contains(header, ",")
}

// isByteRangeRequest reports whether the request asks for a byte range that
// ServeContent would honor: an If-Range header can make the range conditional,
// in which case ServeContent decides whether it applies and this shortcut must
// stand aside.
func isByteRangeRequest(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Range"), "bytes=") && r.Header.Get("If-Range") == ""
}

// s3ErrorInterceptor rewrites the plain-text error responses http.ServeContent
// produces into the S3 XML <Error> document clients parse.
//
// ServeContent is used precisely because it implements Range and the
// conditional headers correctly, but it signals failure with http.Error: a
// text/plain body and no error code. An SDK reading that reports the bare
// status ("416") as the error code instead of InvalidRange, so the status is
// intercepted and the body replaced.
type s3ErrorInterceptor struct {
	http.ResponseWriter

	req *http.Request
	// replaced records that the S3 error document has been written, so
	// ServeContent's own body is discarded rather than appended to it.
	replaced bool
}

// serveContentAPIError maps the failure statuses ServeContent emits to their S3
// errors. 304 is excluded: it is a valid, body-less response, not an error.
func serveContentAPIError(code int) (s3err.APIError, bool) {
	switch code {
	case http.StatusRequestedRangeNotSatisfiable:
		return s3err.InvalidRange, true
	case http.StatusPreconditionFailed:
		return s3err.PreconditionFailed, true
	default:
		return s3err.APIError{}, false
	}
}

func (w *s3ErrorInterceptor) WriteHeader(code int) {
	api, ok := serveContentAPIError(code)
	if !ok {
		w.ResponseWriter.WriteHeader(code)
		return
	}

	w.replaced = true

	// ServeContent sized the response for its own body.
	w.Header().Del("Content-Length")

	s3err.WriteAPI(w.ResponseWriter, w.req, api)
}

func (w *s3ErrorInterceptor) Write(p []byte) (int, error) {
	if w.replaced {
		return len(p), nil
	}

	return w.ResponseWriter.Write(p) //nolint:wrapcheck // Pass the writer's error through unchanged.
}

// streamSeeker adapts a forward-only stream to the io.ReadSeeker http.ServeContent
// requires, using a size the caller already knows (fs.GetObjectResponse.Size,
// which every backend reports).
//
// ServeContent only ever seeks to the end to learn the size, back to the start,
// and then forward to the start of the single range it is about to write. None
// of that needs a real seek: the size is known up front, and moving forward is
// a discard. Seeking is therefore recorded and applied lazily on the next Read,
// so learning the size costs nothing — an eager discard would drain the whole
// stream just to answer it.
//
// Backward movement is impossible on a stream and is reported rather than
// silently serving the wrong bytes; serveObject keeps ServeContent away from
// the one case (multi-range) that would ask for it.
type streamSeeker struct {
	r    io.Reader
	size int64

	// off is where ServeContent believes it is; read is how much of r has
	// actually been consumed. They differ between a Seek and the Read that
	// makes it real.
	off  int64
	read int64
}

func (s *streamSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64

	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = s.off + offset
	case io.SeekEnd:
		abs = s.size + offset
	default:
		return 0, errors.Errorf("invalid whence %d", whence)
	}

	if abs < 0 {
		return 0, errors.Errorf("seek to negative position %d", abs)
	}

	s.off = abs

	return abs, nil
}

func (s *streamSeeker) Read(p []byte) (int, error) {
	if s.off < s.read {
		return 0, errors.Errorf("cannot seek backwards on a streamed object (consumed %d, want %d)", s.read, s.off)
	}

	if skip := s.off - s.read; skip > 0 {
		n, err := io.CopyN(io.Discard, s.r, skip)
		s.read += n

		if err != nil {
			return 0, errors.Wrap(err, "skip to range start")
		}
	}

	if s.off >= s.size {
		return 0, io.EOF
	}

	n, err := s.r.Read(p)
	s.read += int64(n)
	s.off += int64(n)

	return n, err //nolint:wrapcheck // Passing the reader's error (incl. io.EOF) through unchanged.
}

// responseOverrides maps the response-* query parameters onto the headers they
// replace.
var responseOverrides = map[string]string{
	"response-content-type":        "Content-Type",
	"response-content-language":    "Content-Language",
	"response-expires":             "Expires",
	"response-cache-control":       "Cache-Control",
	"response-content-disposition": "Content-Disposition",
	"response-content-encoding":    "Content-Encoding",
}

// applyResponseOverrides replaces representation headers with the values the
// request asked for. An empty value is still an override — a caller asking for
// an empty Content-Disposition means it.
func applyResponseOverrides(h http.Header, q url.Values) {
	for param, header := range responseOverrides {
		if !q.Has(param) {
			continue
		}

		h.Set(header, q.Get(param))
	}
}
