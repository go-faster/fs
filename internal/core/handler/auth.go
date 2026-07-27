package handler

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
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
	// AllowsExplicitly reports whether that permission comes from a grant
	// naming the bucket rather than from a wildcard covering it.
	AllowsExplicitly(accessKey, bucket string, action auth.Action) bool
	// PublicRead reports whether bucket permits anonymous reads.
	PublicRead(bucket string) bool
	// Owner returns the identity an access key acts as, for the <Owner> element
	// of ACL and listing responses.
	Owner(accessKey string) (auth.Identity, bool)
}

// ownerKey is the context key under which the authenticated caller's identity
// is stored.
type ownerKey struct{}

// withOwner returns ctx carrying the authenticated caller's identity.
func withOwner(ctx context.Context, id auth.Identity) context.Context {
	return context.WithValue(ctx, ownerKey{}, id)
}

// callerOwner returns the authenticated caller as an fs.Owner, to record on
// objects they write. Anonymous requests — and handlers running without an
// authenticator — report the anonymous owner, so every object has an owner and
// responses always carry a well-formed <Owner>.
func callerOwner(ctx context.Context) fs.Owner {
	if id, ok := ctx.Value(ownerKey{}).(auth.Identity); ok {
		return fs.Owner{ID: id.UserID, DisplayName: id.DisplayName}
	}

	return fs.Owner{ID: anonymousUserID, DisplayName: anonymousDisplayName}
}

// The owner reported for unauthenticated requests. S3 has no anonymous owner
// concept; these are the canonical values RGW uses.
const (
	anonymousUserID      = "anonymous"
	anonymousDisplayName = "anonymous"
)

// authMiddleware authenticates and authorizes every request before it reaches
// the router. Signed requests (SigV4 header or presigned query) are verified
// and authorized; unsigned requests are allowed only when the target's canned
// ACL permits anonymous access. For signed streaming uploads the request body
// is replaced with a chunk-signature-verifying reader so tampered payloads
// never reach storage.
func authMiddleware(a Authenticator, store fs.Storage, ownerIsolation bool, next http.Handler) http.Handler {
	verifier := sigv4.NewVerifier(a.Secret)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket, key, action := requestScope(r)

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

			id, haveID := a.Owner(res.AccessKey)

			if ownerIsolation && !ownerAllows(r, store, a, res.AccessKey, bucket, action, id) {
				s3err.WriteAPI(w, r, s3err.AccessDenied)
				return
			}

			if res.SignedStreaming() {
				replaceWithVerifiedBody(r, res)
			}

			if haveID {
				r = r.WithContext(withOwner(r.Context(), id))
			}

			next.ServeHTTP(w, r)

			return
		}

		if anonymousAllowed(r.Context(), store, a, bucket, key, action) {
			next.ServeHTTP(w, r)
			return
		}

		s3err.WriteAPI(w, r, s3err.AccessDenied)
	})
}

// requestScope derives the target bucket, key and access level a request needs
// from its method and path (path-style addressing). A root request (ListBuckets)
// has an empty bucket; a bucket-level request has an empty key.
func requestScope(r *http.Request) (bucket, key string, action auth.Action) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ = strings.Cut(path, "/")

	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return bucket, key, auth.ActionRead
	default:
		return bucket, key, auth.ActionWrite
	}
}

// anonymousAllowed decides whether an unsigned request may proceed, consulting
// the stored canned ACLs. It returns true either when access is genuinely
// public or when the target is missing — in the latter case the request is let
// through so the router produces the natural NoSuchBucket/NoSuchKey (404),
// matching S3-compatible (RGW) behavior rather than a blanket 403.
//
//   - service level (no bucket): denied.
//   - bucket level (no key): reads need a public-read bucket; writes (bucket
//     create/delete) are never anonymous.
//   - object level: a missing bucket is let through (→ 404); otherwise writes
//     need a public-read-write bucket and reads need the bucket or the object
//     to be public-read.
func anonymousAllowed(ctx context.Context, store fs.Storage, a Authenticator, bucket, key string, action auth.Action) bool {
	if bucket == "" {
		return false
	}

	if key == "" {
		if action == auth.ActionWrite {
			return false
		}

		level, err := store.BucketACL(ctx, bucket)

		return err == nil && (level.AllowsAnonRead() || a.PublicRead(bucket))
	}

	level, err := store.BucketACL(ctx, bucket)
	if errors.Is(err, fs.ErrBucketNotFound) {
		return true // let the router return NoSuchBucket
	}

	if err != nil {
		return false
	}

	if action == auth.ActionWrite {
		return level.AllowsAnonWrite()
	}

	if level.AllowsAnonRead() || a.PublicRead(bucket) {
		return true
	}

	objACL, err := store.ObjectACL(ctx, bucket, key)
	if err != nil {
		return false
	}

	return objACL.AllowsAnonRead()
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

// ownerAllows applies bucket ownership on top of the grant table: a bucket
// belongs to whoever created it, and another principal reaches it only through
// a grant that names it (see auth.Store.AllowsExplicitly).
//
// Without this, an operator's "*" grant — the natural way to say "the buckets I
// make" — silently means "every bucket anyone makes", and no tenant can keep
// anything to itself. Buckets with no recorded owner (created before ownership,
// or by a backend that does not record it) keep the old behaviour, so nothing
// that works today stops working on upgrade.
func ownerAllows(
	r *http.Request, store fs.Storage, a Authenticator,
	accessKey, bucket string, action auth.Action, id auth.Identity,
) bool {
	if bucket == "" {
		return true
	}

	// Creating a bucket is a claim on a name, not access to someone's data:
	// the answer a caller needs is "that name is taken" (409), which only the
	// operation itself can give. Denying here would report 403 instead.
	if r.Method == http.MethodPut && bucketOnlyRequest(r) {
		return true
	}

	ownership, ok := store.(fs.BucketOwnership)
	if !ok {
		return true
	}

	owner, err := ownership.BucketOwner(r.Context(), bucket)
	if err != nil {
		// The bucket may not exist yet (a create), or the backend may be
		// unable to say. Neither is a reason to deny here: the operation
		// itself reports what happened.
		return true
	}

	if owner.IsZero() || owner.ID == id.UserID {
		return true
	}

	return a.AllowsExplicitly(accessKey, bucket, action)
}

// bucketOnlyRequest reports whether the request addresses a bucket itself
// rather than an object or a subresource of it.
func bucketOnlyRequest(r *http.Request) bool {
	_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	return key == "" && len(r.URL.Query()) == 0
}
