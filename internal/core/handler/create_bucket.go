package handler

import (
	"net/http"
	"strings"

	"github.com/go-faster/fs"
)

// CreateBucket implements PUT /{bucket}. Any LocationConstraint in the request
// body is ignored (this server is single-region); on success it echoes the
// bucket path in the Location header as S3 does. A canned x-amz-acl (e.g.
// public-read) is recorded on the new bucket.
func (h *handler) CreateBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	name, _, _ := strings.Cut(path, "/")

	if err := h.service.CreateBucket(ctx, name); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	if acl := fs.ParseACL(r.Header.Get("X-Amz-Acl")); acl != fs.ACLPrivate {
		if err := h.service.SetBucketACL(ctx, name, acl); err != nil {
			renderError(ctx, w, r, err)
			return
		}
	}

	w.Header().Set("Location", "/"+name)
	w.WriteHeader(http.StatusOK)
}
