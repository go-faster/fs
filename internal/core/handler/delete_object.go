package handler

import (
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

func (h *handler) DeleteObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	query := r.URL.Query()

	// Check if this is an abort multipart upload request.
	if uploadID := query.Get("uploadId"); uploadID != "" {
		err := h.service.AbortMultipartUpload(ctx, bucket, key, uploadID)
		if err != nil {
			renderError(ctx, w, r, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)

		return
	}

	cond, err := requestConditions(r)
	if err != nil {
		s3err.WriteAPI(w, r, s3err.InvalidArgument)
		return
	}

	// Regular delete object. Deleting a key that is already gone is a success
	// in S3 — the operation is idempotent — including when the request carried
	// a condition, which simply has nothing left to guard.
	if err := h.deleteWithConditions(r, bucket, key, cond); err != nil &&
		!errors.Is(err, fs.ErrObjectNotFound) {
		renderError(ctx, w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}
