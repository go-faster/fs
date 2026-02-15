package handler

import (
	"net/http"
	"strings"
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
			renderError(ctx, w, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)

		return
	}

	// Regular delete object.
	err := h.service.DeleteObject(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
