package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/fs"
)

// InitiateMultipartUploadResult represents the response for initiating multipart upload.
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (h *handler) HandleObjectPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	query := r.URL.Query()

	// Check if this is multipart upload initiation.
	if _, ok := query["uploads"]; ok {
		h.initiateMultipartUpload(w, r, bucket, key)
		return
	}

	// Check if this is multipart upload completion.
	if uploadID := query.Get("uploadId"); uploadID != "" {
		h.completeMultipartUpload(w, r, bucket, key, uploadID)
		return
	}

	// Unknown POST operation.
	renderError(ctx, w, fs.ErrUnsupportedOperation)
}

func (h *handler) initiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ctx := r.Context()

	upload, err := h.service.CreateMultipartUpload(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	result := InitiateMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		UploadID: upload.UploadID,
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}
