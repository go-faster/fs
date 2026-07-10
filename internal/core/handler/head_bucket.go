package handler

import (
	"net/http"
	"strings"

	"github.com/go-faster/fs"
)

func (h *handler) HeadBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")

	exists, err := h.service.BucketExists(ctx, bucket)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	if !exists {
		renderError(ctx, w, r, fs.ErrBucketNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
}
