package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
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
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
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
		// The decoded length, not Content-Length. A streaming-signature upload
		// frames the payload in chunks, so Content-Length counts the framing
		// too and the reader above — which strips it — yields fewer bytes than
		// that. A backend that streams exactly the size it is given then runs
		// off the end of the body, which is how every multipart upload from a
		// client using streaming signatures failed against a cluster.
		Size: getDecodedContentLength(r),
	}

	part, err := h.service.UploadPart(ctx, req)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.Header().Set("ETag", `"`+part.ETag+`"`)
	w.WriteHeader(http.StatusOK)
}
