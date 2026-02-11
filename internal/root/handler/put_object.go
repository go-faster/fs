package handler

import (
	"net/http"
	"strings"

	"github.com/go-faster/fs"
)

func (h *handler) PutObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	req := &fs.PutObjectRequest{
		Reader: r.Body,
		Bucket: bucket,
		Key:    key,
		Size:   r.ContentLength,
	}

	if err := h.service.PutObject(ctx, req); err != nil {
		renderError(ctx, w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}
