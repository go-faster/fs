package handler

import (
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-faster/fs"
)

// InitiateMultipartUploadResult represents the response for initiating multipart upload
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// CompleteMultipartUploadResult represents the response for completing multipart upload
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// CompleteMultipartUploadRequest represents the XML request body for completing multipart upload
type CompleteMultipartUploadXML struct {
	XMLName xml.Name           `xml:"CompleteMultipartUpload"`
	Parts   []CompletedPartXML `xml:"Part"`
}

// CompletedPartXML represents a part in the completion request
type CompletedPartXML struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

func (h *handler) HandleBucketPost(w http.ResponseWriter, r *http.Request) {
	// POST to bucket - typically used for delete multiple objects
	query := r.URL.Query()

	// Handle delete multiple objects operation
	if _, ok := query["delete"]; ok {
		// For now, just acknowledge the delete request
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)

		return
	}

	// Unknown POST operation to bucket
	w.WriteHeader(http.StatusOK)
}

func (h *handler) HandleObjectPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	query := r.URL.Query()

	// Check if this is multipart upload initiation
	if _, ok := query["uploads"]; ok {
		h.initiateMultipartUpload(w, r, bucket, key)
		return
	}

	// Check if this is multipart upload completion
	if uploadID := query.Get("uploadId"); uploadID != "" {
		h.completeMultipartUpload(w, r, bucket, key, uploadID)
		return
	}

	// Unknown POST operation
	renderError(ctx, w, nil)
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

func (h *handler) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	ctx := r.Context()

	// Parse the XML body
	var xmlReq CompleteMultipartUploadXML
	if err := xml.NewDecoder(r.Body).Decode(&xmlReq); err != nil {
		renderError(ctx, w, err)
		return
	}

	// Convert to internal format
	parts := make([]fs.CompletedPart, len(xmlReq.Parts))
	for i, p := range xmlReq.Parts {
		// Remove quotes from ETag if present
		etag := strings.Trim(p.ETag, `"`)
		parts[i] = fs.CompletedPart{
			PartNumber: p.PartNumber,
			ETag:       etag,
		}
	}

	// Sort parts by part number
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	req := &fs.CompleteMultipartUploadRequest{
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
		Parts:    parts,
	}

	resp, err := h.service.CompleteMultipartUpload(ctx, req)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	result := CompleteMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Location: resp.Location,
		Bucket:   resp.Bucket,
		Key:      resp.Key,
		ETag:     `"` + resp.ETag + `"`,
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}

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

	// Handle AWS chunked encoding
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
