package handler

import (
	"io"
	"net/http"
)

// drainLimit is how much of a request body drainBody reads past what the
// handler did. A refused part of up to 64 MiB, more than any SDK's default
// part size, reaches the client as its error.
const drainLimit = 64 << 20

// drainBody reads what the handler left of the request body, after the
// response is sent, before the connection is given back.
//
// A handler that refuses a write before reading it — no such bucket, a quota,
// a bad signature — answers while the client is still sending. Go's server
// then closes the connection on the unread bytes, the kernel answers what
// arrives next with a reset, and the client sees that instead of the
// response: aws-sdk-go-v2 sends Expect: 100-continue for bodies of 2 MiB and
// up but never waits for the 100, and so gets a reset and retries a request
// that cannot succeed. Draining lets the body arrive and the response be read.
//
// It reads after the response, never before: reading first would send the
// 100 Continue to a client that does wait, inviting the very body the server
// is about to refuse. Once the response is written no 100 is sent, and such a
// client closes the connection rather than send the body, which ends the
// drain. A body declared larger than drainLimit is not read at all; it would
// be cut off anyway.
//
// The response is flushed first so such a client has it while the server
// waits; S3 errors declare their length for this (s3err.WriteAPI).
func drainBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := r.Body

		next.ServeHTTP(w, r)

		if body == nil || body == http.NoBody || r.ContentLength > drainLimit {
			return
		}

		// Only a response of declared length is complete once flushed; any
		// other is ended when the handler returns, and a client waiting for
		// its end before sending would wait on the drain.
		if w.Header().Get("Content-Length") != "" {
			_ = http.NewResponseController(w).Flush()
		}

		_, _ = io.CopyN(io.Discard, body, drainLimit)
	})
}
