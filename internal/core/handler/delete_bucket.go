package handler

import (
	"net/http"
	"strings"
)

func (h *handler) DeleteBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	name, _, _ := strings.Cut(path, "/")

	err := h.service.DeleteBucket(ctx, name)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
