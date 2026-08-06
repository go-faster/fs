package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
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
	renderError(ctx, w, r, fs.ErrUnsupportedOperation)
}

func (h *handler) initiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ctx := r.Context()

	tags, err := parseTaggingHeader(r.Header.Get("X-Amz-Tagging"))
	if err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
		return
	}

	upload, err := h.service.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket:   bucket,
		Key:      key,
		Metadata: extractObjectMetadata(r.Header),
		Tags:     tags,
		ACL:      fs.ParseACL(r.Header.Get("X-Amz-Acl")),
		Owner:    callerOwner(ctx),

		// Settled here rather than at completion: by then the parts are
		// already on disk, and would have been staged in the clear.
		ServerSideEncryption: h.requestedEncryption(r, bucket),

		// Same reason for the checksum: the parts are digested as they arrive,
		// so what to digest them with has to be known before the first one
		// does. Only the algorithm is taken from the request here — a create
		// carries no body, so there is no digest to check against.
		ChecksumAlgorithm: uploadChecksumAlgorithm(r),
		ChecksumType:      strings.TrimSpace(r.Header.Get(checksumTypeHeader)),
	})
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	result := InitiateMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   upload.Bucket,
		Key:      upload.Key,
		UploadID: upload.UploadID,
	}

	w.Header().Set("Content-Type", "application/xml")
	writeSSE(w, h.requestedEncryption(r, bucket))

	// Echoed as headers, which is where the S3 API puts them on a create: a
	// client that asked by algorithm alone learns which type it got, and one
	// that asked by neither sees nothing at all.
	if upload.ChecksumAlgorithm != "" {
		w.Header().Set(checksumAlgorithmHeader, upload.ChecksumAlgorithm)
	}

	if upload.ChecksumType != "" {
		w.Header().Set(checksumTypeHeader, upload.ChecksumType)
	}

	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}
