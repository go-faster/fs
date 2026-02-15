package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// InitiateMultipartUploadResult represents the response for initiating multipart upload
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// CompleteMultipartUploadResult represents the response for completing multipart upload
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (h *handler) HandleBucketPost(w http.ResponseWriter, r *http.Request) {
	// POST to bucket - typically used for delete multiple objects
	// For now, just return OK
	w.WriteHeader(http.StatusOK)
}

func (h *handler) HandleObjectPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	query := r.URL.Query()

	// Check if this is multipart upload initiation
	if _, ok := query["uploads"]; ok {
		// Initiate multipart upload
		uploadID := uuid.New().String()

		result := InitiateMultipartUploadResult{
			Bucket:   bucket,
			Key:      key,
			UploadID: uploadID,
		}

		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		if err := xml.NewEncoder(w).Encode(result); err != nil {
			renderError(ctx, w, err)
		}
		return
	}

	// Check if this is multipart upload completion
	if uploadID := query.Get("uploadId"); uploadID != "" {
		// Complete multipart upload - for simplicity, just return success
		result := CompleteMultipartUploadResult{
			Location: "/" + bucket + "/" + key,
			Bucket:   bucket,
			Key:      key,
			ETag:     `"` + uploadID + `"`,
		}

		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		if err := xml.NewEncoder(w).Encode(result); err != nil {
			renderError(ctx, w, err)
		}
		return
	}

	// Unknown POST operation
	w.WriteHeader(http.StatusMethodNotAllowed)
}
