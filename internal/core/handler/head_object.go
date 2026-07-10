package handler

import (
	"net/http"
	"strings"
)

func (h *handler) HeadObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	resp, err := h.service.GetObject(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	// serveObject is HEAD-safe: it sets headers (Content-Type, ETag, Content-Length,
	// Last-Modified) and honors conditional requests without writing a body.
	serveObject(w, r, key, resp)
}
