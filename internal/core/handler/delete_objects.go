package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/fs"
)

// DeleteObjectsRequest represents the XML request body for deleting multiple objects.
type DeleteObjectsRequest struct {
	XMLName xml.Name         `xml:"Delete"`
	Objects []ObjectToDelete `xml:"Object"`
	Quiet   bool             `xml:"Quiet"`
}

// ObjectToDelete represents an object to be deleted.
type ObjectToDelete struct {
	Key       string `xml:"Key"`
	VersionId string `xml:"VersionId,omitempty"`
}

// DeleteObjectsResult represents the response for deleting multiple objects.
type DeleteObjectsResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	Xmlns   string          `xml:"xmlns,attr"`
	Deleted []DeletedObject `xml:"Deleted,omitempty"`
	Errors  []DeleteError   `xml:"Error,omitempty"`
}

// DeletedObject represents a successfully deleted object.
type DeletedObject struct {
	Key                   string `xml:"Key"`
	VersionId             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionId string `xml:"DeleteMarkerVersionId,omitempty"`
}

// DeleteError represents an error deleting an object.
type DeleteError struct {
	Key       string `xml:"Key"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
	VersionId string `xml:"VersionId,omitempty"`
}

func (h *handler) HandleBucketPost(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")
	query := r.URL.Query()

	// Handle delete multiple objects operation.
	if _, ok := query["delete"]; ok {
		h.deleteObjects(w, r, bucket)
		return
	}

	// Unknown POST operation to bucket.
	ctx := r.Context()
	renderError(ctx, w, fs.ErrUnsupportedOperation)
}

func (h *handler) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx := r.Context()

	// Parse the XML body.
	var req DeleteObjectsRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		renderError(ctx, w, err)
		return
	}

	result := DeleteObjectsResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}

	// Delete each object.
	for _, obj := range req.Objects {
		err := h.service.DeleteObject(ctx, bucket, obj.Key)
		if err != nil {
			// Add error to result.
			result.Errors = append(result.Errors, DeleteError{
				Key:     obj.Key,
				Code:    "InternalError",
				Message: err.Error(),
			})
		} else if !req.Quiet {
			// Add deleted object to result (only if not in quiet mode).
			result.Deleted = append(result.Deleted, DeletedObject{
				Key: obj.Key,
			})
		}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}
