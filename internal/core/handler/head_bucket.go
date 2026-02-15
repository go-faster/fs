package handler

import (
	"net/http"
	"strings"
)

func (h *handler) HeadBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")

	exists, err := h.service.BucketExists(ctx, bucket)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
}
