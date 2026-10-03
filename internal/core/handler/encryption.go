package handler

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 names the SSE-C key by its MD5.
	"crypto/subtle"
	"encoding/base64"
	"net/http"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// sseHeader is the S3 header naming the server-side encryption algorithm, on
// the request and echoed on the response.
const sseHeader = "x-amz-server-side-encryption"

// requestedEncryption resolves the algorithm a write should use, most
// specific first: the request header, then the bucket's ?encryption default,
// then the server-wide default.
//
// That order is S3's, and it is what makes a default safe to turn on: a client
// that names an algorithm gets exactly it, and the storage layer refuses
// anything it cannot honor rather than storing plaintext under a header that
// claims otherwise.
func (h *handler) requestedEncryption(r *http.Request, bucket string) string {
	if v := r.Header.Get(sseHeader); v != "" {
		return v
	}

	// A customer key is the request naming its encryption: defaults yield.
	if fs.CustomerKeyFrom(r.Context()) != nil {
		return ""
	}

	if v := h.bucketDefaultEncryption(r, bucket); v != "" {
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

// SSE-C headers: the key's algorithm, the key, and its MD5, on the request
// for the object itself, and with this prefix for a copy's source.
const (
	customerPrefix   = "x-amz-server-side-encryption-customer-"
	copySourcePrefix = "x-amz-copy-source-server-side-encryption-customer-"
)

// customerKey reads the SSE-C headers with prefix: nil when there are none.
func customerKey(r *http.Request, prefix string) (fs.CustomerKey, *s3err.APIError) {
	alg := r.Header.Get(prefix + "algorithm")
	key := r.Header.Get(prefix + "key")
	sum := r.Header.Get(prefix + "key-MD5")

	if alg == "" && key == "" && sum == "" {
		return nil, nil
	}

	invalid := func(msg string) *s3err.APIError {
		e := s3err.InvalidArgument
		e.Message = msg

		return &e
	}

	if alg != "AES256" {
		return nil, invalid("The requested encryption algorithm is not valid, it must be AES256.")
	}

	k, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(k) != 32 {
		return nil, invalid("The secret key was invalid for the specified algorithm.")
	}

	want := md5.Sum(k) //nolint:gosec // S3's key checksum.
	if got, err := base64.StdEncoding.DecodeString(sum); err != nil || subtle.ConstantTimeCompare(got, want[:]) != 1 {
		return nil, invalid("The calculated MD5 hash of the key did not match the hash that was provided.")
	}

	return k, nil
}

// secure reports whether r reached the server over TLS, here or at a proxy
// in front of it. Trusting the proxy's header lets a client lie only about
// its own key's exposure.
func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// withCustomerKey puts the request's SSE-C key on its context and echoes the
// key's headers, as S3 does, or writes the error and reports false.
func (h *handler) withCustomerKey(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	k, apiErr := customerKey(r, customerPrefix)
	if apiErr == nil && k == nil {
		_, apiErr = customerKey(r, copySourcePrefix)
		if apiErr == nil && r.Header.Get(copySourcePrefix+"key") == "" {
			return r, true
		}
	}

	if apiErr != nil {
		s3err.WriteAPI(w, r, *apiErr)

		return r, false
	}

	if !secure(r) && !h.customerKeysOverHTTP {
		e := s3err.InvalidRequest
		e.Message = "Requests specifying Server Side Encryption with Customer provided keys must be made over a secure connection."
		s3err.WriteAPI(w, r, e)

		return r, false
	}

	if k == nil {
		return r, true // Only a copy source's key, which the copy reads.
	}

	if r.Header.Get(sseHeader) != "" {
		e := s3err.InvalidArgument
		e.Message = "Server Side Encryption with Customer provided key is incompatible with the encryption method specified."
		s3err.WriteAPI(w, r, e)

		return r, false
	}

	w.Header().Set(customerPrefix+"algorithm", "AES256")
	w.Header().Set(customerPrefix+"key-MD5", r.Header.Get(customerPrefix+"key-MD5"))

	return r.WithContext(fs.WithCustomerKey(r.Context(), k)), true
}

// copySourceContext is the context a copy reads its source with: the copy
// source's SSE-C key in place of the destination's. Its headers were checked
// by withCustomerKey.
func copySourceContext(r *http.Request) context.Context {
	k, _ := customerKey(r, copySourcePrefix)

	return fs.WithCustomerKey(r.Context(), k)
}
