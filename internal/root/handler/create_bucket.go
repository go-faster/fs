package handler

import (
	"net/http"
	"strings"
)

func (h *handler) CreateBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	name, _, _ := strings.Cut(path, "/")

	err := h.service.CreateBucket(ctx, name)
	if err != nil {
		renderError(ctx, w, err)
		return
	}
}
