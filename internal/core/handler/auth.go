package handler

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/s3err"
	"github.com/go-faster/fs/internal/sigv4"
)

// Authenticator verifies credentials and authorizes S3 operations. It is
// satisfied by *auth.Store.
type Authenticator interface {
	// Secret resolves an access key to its secret (for signature verification).
	Secret(accessKey string) (secret string, ok bool)
	// Allow reports whether the access key may perform action on bucket.
	Allow(accessKey, bucket string, action auth.Action) bool
	// PublicRead reports whether bucket permits anonymous reads.
	PublicRead(bucket string) bool
}

// authMiddleware authenticates and authorizes every request before it reaches
// the router. Signed requests (SigV4 header or presigned query) are verified
// and authorized; unsigned requests are allowed only as reads of a public-read
// bucket. For signed streaming uploads the request body is replaced with a
// chunk-signature-verifying reader so tampered payloads never reach storage.
func authMiddleware(a Authenticator, next http.Handler) http.Handler {
	verifier := sigv4.NewVerifier(a.Secret)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket, action := requestScope(r)

		if hasSigV4Credentials(r) {
			res, err := verifier.Verify(r)
			if err != nil {
				writeAuthError(w, r, err)
				return
			}

			if !a.Allow(res.AccessKey, bucket, action) {
				s3err.WriteAPI(w, r, s3err.AccessDenied)
				return
			}

			if res.SignedStreaming() {
				replaceWithVerifiedBody(r, res)
			}

			next.ServeHTTP(w, r)

			return
		}

		// Anonymous access: only reads of an explicitly public-read bucket.
		if action == auth.ActionRead && bucket != "" && a.PublicRead(bucket) {
			next.ServeHTTP(w, r)
			return
		}

		s3err.WriteAPI(w, r, s3err.AccessDenied)
	})
}

// requestScope derives the target bucket and the access level a request needs
// from its method and path (path-style addressing). A root request (ListBuckets)
// has an empty bucket.
func requestScope(r *http.Request) (bucket string, action auth.Action) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ = strings.Cut(path, "/")

	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return bucket, auth.ActionRead
	default:
		return bucket, auth.ActionWrite
	}
}

// hasSigV4Credentials reports whether the request carries SigV4 auth (an
// AWS4-HMAC-SHA256 Authorization header or a presigned query signature).
func hasSigV4Credentials(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") ||
		r.URL.Query().Get("X-Amz-Algorithm") != ""
}

// replaceWithVerifiedBody swaps the request body for a reader that decodes the
// aws-chunked framing and verifies each chunk signature, then strips the
// streaming markers so the downstream handler treats the body as a plain,
// already-decoded payload.
func replaceWithVerifiedBody(r *http.Request, res *sigv4.Result) {
	r.Body = io.NopCloser(res.ChunkVerifyingReader(r.Body))
	r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

	if ce := cleanContentEncoding(r.Header.Get("Content-Encoding")); ce == "" {
		r.Header.Del("Content-Encoding")
	} else {
		r.Header.Set("Content-Encoding", ce)
	}
}

// writeAuthError maps a sigv4 verification error to its S3 error response.
func writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sigv4.ErrSignatureMismatch):
		s3err.WriteAPI(w, r, s3err.SignatureDoesNotMatch)
	case errors.Is(err, sigv4.ErrUnknownAccessKey):
		s3err.WriteAPI(w, r, s3err.InvalidAccessKeyID)
	case errors.Is(err, sigv4.ErrRequestExpired):
		s3err.WriteAPI(w, r, s3err.ExpiredPresignedRequest)
	case errors.Is(err, sigv4.ErrClockSkew):
		s3err.WriteAPI(w, r, s3err.RequestTimeTooSkewed)
	case errors.Is(err, sigv4.ErrMalformedSignature):
		s3err.WriteAPI(w, r, s3err.AuthHeaderMalformed)
	case errors.Is(err, sigv4.ErrMissingSignature):
		s3err.WriteAPI(w, r, s3err.MissingSecurityHeader)
	default:
		s3err.WriteAPI(w, r, s3err.AccessDenied)
	}
}
