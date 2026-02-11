package handler

import (
	"context"
	"net/http"
)

func renderError(ctx context.Context, w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
