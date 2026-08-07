package handler

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/s3err"
)

// renderError logs err and writes the corresponding S3 XML error response,
// mapping the fs.Err* sentinels to their S3 codes.
func renderError(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	logRequestError(ctx, r, s3err.FromError(err).Code, err)

	s3err.Write(w, r, err)
}

// renderAPIError logs and writes a specific S3 error (used where the handler
// knows the exact code, e.g. MalformedXML or InvalidPartOrder).
func renderAPIError(ctx context.Context, w http.ResponseWriter, r *http.Request, api s3err.APIError, err error) {
	logRequestError(ctx, r, api.Code, err)

	s3err.WriteAPI(w, r, api)
}

// logRequestError records a failed request at the level the failure deserves,
// naming what the client asked for.
//
// NoSuchBucket and NoSuchKey are logged at debug rather than error. They are
// ordinary client outcomes, not server faults: a HEAD that probes whether a key
// exists, a GET of something already deleted, an SDK that checks for a bucket
// before creating it. At error level they are the loudest thing in the log on a
// healthy server, which trains an operator to ignore the level that is supposed
// to mean something is wrong.
//
// The bucket and key are attached to every failure, not only those two. A code
// on its own says what went wrong and never what it went wrong on, which is the
// first thing anyone reading the line wants — and the request path is right
// there.
func logRequestError(ctx context.Context, r *http.Request, code string, err error) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	fields := []zap.Field{
		zap.String("code", code),
		zap.String("bucket", bucket),
	}

	// Omitted rather than empty on a bucket-level request, where there is no
	// key and a blank one reads as though the client sent one.
	if key != "" {
		fields = append(fields, zap.String("key", key))
	}

	fields = append(fields, zap.Error(err))

	lg := zctx.From(ctx)

	if code == s3err.NoSuchBucket.Code || code == s3err.NoSuchKey.Code {
		lg.Debug("Request failed", fields...)

		return
	}

	lg.Error("Request failed", fields...)
}
