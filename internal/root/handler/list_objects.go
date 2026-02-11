package handler

import (
	"encoding/xml"
	"net/http"
	"strings"
)

func (h *handler) ListObjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")
	prefix := r.URL.Query().Get("prefix")

	objects, err := h.service.ListObjects(ctx, bucket, prefix)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	objectInfos := make([]ObjectInfo, len(objects))
	for i, obj := range objects {
		objectInfos[i] = ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			ETag:         obj.ETag,
		}
	}

	response := ListBucketResult{
		Name:     bucket,
		Contents: objectInfos,
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		renderError(ctx, w, err)
		return
	}
	if err := xml.NewEncoder(w).Encode(response); err != nil {
		renderError(ctx, w, err)
		return
	}
}
