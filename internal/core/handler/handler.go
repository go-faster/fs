package handler

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

type handler struct {
	service fs.Storage
}

// Option configures the handler built by New.
type Option func(*options)

type options struct {
	authenticator Authenticator
	cors          CORSResolver
}

// WithAuthenticator enables SigV4 authentication and grant-based authorization
// using a. Without it (the library default) the handler serves anonymously.
func WithAuthenticator(a Authenticator) Option {
	return func(o *options) { o.authenticator = a }
}

// WithCORS enables per-bucket CORS: OPTIONS preflight handling and CORS
// response headers on cross-origin requests, resolved through c.
func WithCORS(c CORSResolver) Option {
	return func(o *options) { o.cors = c }
}

// New returns the S3-compatible http.Handler for a storage service. Every
// response carries an x-amz-request-id header; request routing is delegated to
// route. Options enable authentication and CORS.
//
// Middleware order (outermost first): request-id → CORS → auth → router, so
// error responses carry a request id, CORS preflight is answered before auth,
// and only authenticated (or public-read) requests reach the router.
func New(s fs.Storage, opts ...Option) http.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	h := handler{service: s}

	mux := http.NewServeMux()
	mux.HandleFunc("/", h.route)

	var inner http.Handler = mux
	if o.authenticator != nil {
		inner = authMiddleware(o.authenticator, s, inner)
	}

	if o.cors != nil {
		inner = corsMiddleware(o.cors, inner)
	}

	return withRequestID(inner)
}

// withRequestID stamps every response with a unique x-amz-request-id (echoed
// into S3 error bodies) and a Server header, matching what S3 clients expect.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-request-id", newRequestID())
		w.Header().Set("Server", "go-faster/fs")
		next.ServeHTTP(w, r)
	})
}

// newRequestID returns a random 16-hex-character request identifier.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}

	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// route dispatches a request to the appropriate handler based on the path shape
// (root / bucket / object) and method. Unsupported methods and operations
// return the corresponding S3 XML error.
func (h *handler) route(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	zctx.From(ctx).Debug("Received request",
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("query", r.URL.RawQuery),
	)

	path := strings.TrimPrefix(r.URL.Path, "/")

	// Root path: only ListBuckets.
	if path == "" {
		if r.Method == http.MethodGet {
			h.ListBuckets(w, r)
			return
		}

		s3err.WriteAPI(w, r, s3err.MethodNotAllowed)

		return
	}

	if _, key, _ := strings.Cut(path, "/"); key == "" {
		h.routeBucket(w, r)
		return
	}

	h.routeObject(w, r)
}

// routeBucket handles requests addressed at a bucket (no object key).
func (h *handler) routeBucket(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	switch r.Method {
	case http.MethodGet:
		switch {
		case q.Has("location"):
			h.GetBucketLocation(w, r)
		case q.Has("versions"):
			h.ListObjectVersions(w, r)
		case q.Has("uploads"):
			h.ListMultipartUploads(w, r)
		case hasUnsupportedBucketSubresource(q):
			s3err.WriteAPI(w, r, s3err.NotImplemented)
		case q.Get("list-type") == "2":
			h.ListObjectsV2(w, r)
		default:
			h.ListObjectsV1(w, r)
		}
	case http.MethodPut:
		if hasUnsupportedBucketSubresource(q) {
			s3err.WriteAPI(w, r, s3err.NotImplemented)
			return
		}

		h.CreateBucket(w, r)
	case http.MethodHead:
		h.HeadBucket(w, r)
	case http.MethodDelete:
		if hasUnsupportedBucketSubresource(q) {
			s3err.WriteAPI(w, r, s3err.NotImplemented)
			return
		}

		h.DeleteBucket(w, r)
	case http.MethodPost:
		// POST to a bucket initiates DeleteObjects (?delete).
		h.HandleBucketPost(w, r)
	default:
		s3err.WriteAPI(w, r, s3err.MethodNotAllowed)
	}
}

// routeObject handles requests addressed at an object key.
func (h *handler) routeObject(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	switch r.Method {
	case http.MethodGet:
		switch {
		case q.Has("uploadId"):
			h.ListParts(w, r)
		case q.Has("tagging"):
			h.GetObjectTagging(w, r)
		default:
			h.GetObject(w, r)
		}
	case http.MethodPut:
		if q.Has("tagging") {
			h.PutObjectTagging(w, r)
			return
		}

		h.PutObject(w, r)
	case http.MethodHead:
		h.HeadObject(w, r)
	case http.MethodDelete:
		if q.Has("tagging") {
			h.DeleteObjectTagging(w, r)
			return
		}

		h.DeleteObject(w, r)
	case http.MethodPost:
		// POST to an object path drives multipart upload initiation/completion.
		h.HandleObjectPost(w, r)
	default:
		s3err.WriteAPI(w, r, s3err.MethodNotAllowed)
	}
}

// unsupportedBucketSubresources are query parameters for bucket features the
// server does not implement; requests carrying them get a NotImplemented error
// rather than being misinterpreted as a plain listing or create.
var unsupportedBucketSubresources = []string{
	"accelerate", "acl", "analytics", "cors", "encryption", "inventory",
	"lifecycle", "logging", "metrics", "notification", "object-lock",
	"ownershipControls", "policy", "policyStatus", "publicAccessBlock",
	"replication", "requestPayment", "tagging", "versioning", "website",
}

func hasUnsupportedBucketSubresource(q map[string][]string) bool {
	for _, name := range unsupportedBucketSubresources {
		if _, ok := q[name]; ok {
			return true
		}
	}

	return false
}
