package server

import (
	"io"
	"net/http"
	"time"
)

// http.Server's ReadTimeout and WriteTimeout are deadlines on the *whole*
// request and the *whole* response, body included. For a server whose job is
// to move objects, that is a size limit wearing a clock's clothing: a transfer
// is cut off once it has taken longer than the timeout, however fast it was
// going. At the 30s default, a GET is truncated unless the client sustains
// size/30s — 1 MB/s for a 30 MB object, 34 MB/s for a 1 GB one — and the
// client sees a body short of the Content-Length it was promised, which is
// data loss that looks like a completed download.
//
// The deadline that is actually wanted is on *stalling*, not on duration: cut
// off a peer that has stopped making progress, and let a slow but moving one
// run as long as it needs. So the http.Server fields are left unset and the
// deadlines are re-armed here as the bytes flow.
//
// Every read and every write re-arms, rather than only those far enough apart
// to be worth a timer update. Throttling the re-arm saves little and leaves a
// window where the deadline lapses while the handler is between calls — the
// transfer is then cut off for being slow after all, which is the bug this
// file exists to remove. So the rule is exactly "no single read or write may
// block longer than the timeout", with nothing to reason about beyond it.

// withProgressDeadlines re-arms the connection's read and write deadlines for
// as long as the request body and the response body keep moving, so that
// timeout means "this peer stopped" rather than "this transfer took a while".
//
// Each deadline is armed immediately before the transfer it guards, never up
// front: a deadline armed before the handler runs would be spent by the time a
// handler that thinks before it answers — a multipart completion assembling
// its parts, say — produced its first byte, and would cut off the response it
// was supposed to protect. Nothing is left unguarded by arming late, because a
// deadline is only needed once there are bytes to move.
func withProgressDeadlines(next http.Handler, readTimeout, writeTimeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)

		if readTimeout > 0 && r.Body != nil {
			r.Body = &progressReader{ReadCloser: r.Body, rc: rc, timeout: readTimeout}
		}

		if writeTimeout > 0 {
			w = &progressWriter{ResponseWriter: w, rc: rc, timeout: writeTimeout}
		}

		next.ServeHTTP(w, r)
	})
}

// progressWriter pushes the write deadline back as the response body is
// written.
type progressWriter struct {
	http.ResponseWriter

	rc      *http.ResponseController
	timeout time.Duration
}

// Unwrap lets http.ResponseController and the handler's own ResponseWriter
// wrappers reach the writer underneath.
func (w *progressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *progressWriter) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.timeout))

	return w.ResponseWriter.Write(p) //nolint:wrapcheck // Pass the writer's error through unchanged.
}

// progressReader pushes the read deadline back as the request body is read.
type progressReader struct {
	io.ReadCloser

	rc      *http.ResponseController
	timeout time.Duration
}

func (r *progressReader) Read(p []byte) (int, error) {
	_ = r.rc.SetReadDeadline(time.Now().Add(r.timeout))

	return r.ReadCloser.Read(p) //nolint:wrapcheck // Pass the body's error through unchanged.
}
