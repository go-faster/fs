package handler

import (
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (h *handler) HeadBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")

	// Check if bucket exists by listing objects (with empty prefix, limit 1)
	_, err := h.service.ListObjects(ctx, bucket, "")
	if err != nil {
		if errors.Is(err, fs.ErrBucketNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		renderError(ctx, w, err)

		return
	}

	w.WriteHeader(http.StatusOK)
}
