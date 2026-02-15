package handler

import (
	"net/http"
	"strconv"
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

	defer func() { _ = resp.Reader.Close() }()

	// Set headers
	w.Header().Set("Content-Length", strconv.FormatInt(resp.Size, 10))
	w.Header().Set("Last-Modified", resp.LastModified.UTC().Format(http.TimeFormat))

	if resp.ETag != "" {
		w.Header().Set("ETag", `"`+resp.ETag+`"`)
	}

	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}

	w.WriteHeader(http.StatusOK)
}
