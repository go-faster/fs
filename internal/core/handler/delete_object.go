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

	// A delete on a versioned bucket writes a tombstone rather than removing
	// anything, and a delete naming a version removes exactly that one. Both
	// go through the same call, which also handles the unversioned case, so
	// the handler does not have to ask about bucket state first.
	if versioner, ok := h.service.(fs.Versioner); ok && cond.IsZero() {
		result, err := versioner.DeleteObjectVersion(ctx, bucket, key, query.Get("versionId"))

		switch {
		case err == nil:
			writeDeleteResult(w, result)
			w.WriteHeader(http.StatusNoContent)

			return
		case errors.Is(err, fs.ErrObjectNotFound):
			// Deleting what is already gone is a success in S3.
			w.WriteHeader(http.StatusNoContent)

			return
		case !errors.Is(err, fs.ErrUnsupportedOperation):
			renderError(ctx, w, r, err)

			return
		}
		// A backend that cannot version falls through to the plain delete.
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

// writeDeleteResult reports what the delete did. S3 uses these two headers to
// let a client tell "the key is gone" from "the key now has a tombstone, and
// here is the id you would delete to undo it".
func writeDeleteResult(w http.ResponseWriter, result fs.DeleteResult) {
	writeVersionID(w, result.VersionID)

	if result.DeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
	}
}
