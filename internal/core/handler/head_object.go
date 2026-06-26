package handler

import (
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (h *handler) HeadObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	resp, err := h.service.GetObject(ctx, bucket, key)
	if err != nil {
		if errors.Is(err, fs.ErrBucketNotFound) || errors.Is(err, fs.ErrObjectNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		renderError(ctx, w, err)

		return
	}

	// serveObject is HEAD-safe: it sets headers (Content-Type, ETag, Content-Length,
	// Last-Modified) and honors conditional requests without writing a body.
	serveObject(w, r, key, resp)
}
