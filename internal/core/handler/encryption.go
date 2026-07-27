package handler

import (
	"net/http"
)

// sseHeader is the S3 header naming the server-side encryption algorithm, on
// the request and echoed on the response.
const sseHeader = "x-amz-server-side-encryption"

// requestedEncryption resolves the algorithm a write should use: what the
// request asks for, falling back to the server's default.
//
// The header wins over the default in both directions, which is what makes a
// default safe to turn on: a client that names an algorithm gets it, and the
// storage layer refuses anything it cannot honor rather than storing
// plaintext under a header that claims otherwise.
func (h *handler) requestedEncryption(r *http.Request) string {
	if v := r.Header.Get(sseHeader); v != "" {
		return v
	}

	return h.defaultEncryption
}

// writeSSE reports the algorithm an object is encrypted with. It is omitted
// entirely for an unencrypted object, which is how S3 says "not encrypted" —
// a client reads the header's absence, not a placeholder value.
func writeSSE(w http.ResponseWriter, algorithm string) {
	if algorithm != "" {
		w.Header().Set(sseHeader, algorithm)
	}
}
