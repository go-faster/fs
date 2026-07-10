package handler

import (
	"context"
	"net/http"

	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/s3err"
)

// renderError logs err and writes the corresponding S3 XML error response,
// mapping the fs.Err* sentinels to their S3 codes.
func renderError(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	zctx.From(ctx).Error("Request failed",
		zap.String("code", s3err.FromError(err).Code),
		zap.Error(err),
	)

	s3err.Write(w, r, err)
}

// renderAPIError logs and writes a specific S3 error (used where the handler
// knows the exact code, e.g. MalformedXML or InvalidPartOrder).
func renderAPIError(ctx context.Context, w http.ResponseWriter, r *http.Request, api s3err.APIError, err error) {
	zctx.From(ctx).Error("Request failed",
		zap.String("code", api.Code),
		zap.Error(err),
	)

	s3err.WriteAPI(w, r, api)
}
