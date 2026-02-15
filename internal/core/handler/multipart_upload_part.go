package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/fs"
)

func (h *handler) UploadPart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	query := r.URL.Query()

	uploadID := query.Get("uploadId")
	partNumberStr := query.Get("partNumber")

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	// Handle AWS chunked encoding.
	reader := getBodyReader(r)

	req := &fs.UploadPartRequest{
		Bucket:     bucket,
		Key:        key,
		UploadID:   uploadID,
		PartNumber: partNumber,
		Reader:     reader,
		Size:       r.ContentLength,
	}

	part, err := h.service.UploadPart(ctx, req)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	w.Header().Set("ETag", `"`+part.ETag+`"`)
	w.WriteHeader(http.StatusOK)
}
