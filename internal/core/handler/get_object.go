package handler

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (h *handler) GetObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	resp, err := h.service.GetObject(ctx, bucket, key)
	if errors.Is(err, fs.ErrObjectNotFound) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if errors.Is(err, fs.ErrBucketNotFound) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err != nil {
		renderError(ctx, w, err)
		return
	}

	defer func() {
		_ = resp.Reader.Close()
	}()

	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	if resp.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.Size, 10))
	}

	if resp.ETag != "" {
		w.Header().Set("ETag", resp.ETag)
	}

	if !resp.LastModified.IsZero() {
		w.Header().Set("Last-Modified", resp.LastModified.UTC().Format(http.TimeFormat))
	}

	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, resp.Reader); err != nil {
		// Cannot render error here as headers are already sent
		return
	}
}
