package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// maxDeleteObjects is the number of keys S3 accepts in one DeleteObjects
// request.
const maxDeleteObjects = 1000

// DeleteObjectsRequest represents the XML request body for deleting multiple objects.
type DeleteObjectsRequest struct {
	XMLName xml.Name         `xml:"Delete"`
	Objects []ObjectToDelete `xml:"Object"`
	Quiet   bool             `xml:"Quiet"`
}

// ObjectToDelete represents an object to be deleted. ETag, Size and
// LastModifiedTime are the per-object conditional-delete guards: when present,
// the key is deleted only while it still matches them.
type ObjectToDelete struct {
	Key              string `xml:"Key"`
	VersionId        string `xml:"VersionId,omitempty"`
	ETag             string `xml:"ETag,omitempty"`
	Size             *int64 `xml:"Size,omitempty"`
	LastModifiedTime string `xml:"LastModifiedTime,omitempty"`
}

// conditions renders the object's guards as storage conditions. A malformed
// timestamp is reported rather than dropped, so an unenforceable guard fails
// the entry instead of silently deleting unconditionally.
func (o ObjectToDelete) conditions() (fs.Conditions, error) {
	cond := fs.Conditions{IfMatch: o.ETag, Size: o.Size}

	if o.LastModifiedTime != "" {
		t, err := http.ParseTime(o.LastModifiedTime)
		if err != nil {
			return fs.Conditions{}, errors.Wrap(errMalformedCondition, "LastModifiedTime")
		}

		cond.LastModified = &t
	}

	return cond, nil
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
	renderError(ctx, w, r, fs.ErrUnsupportedOperation)
}

func (h *handler) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx := r.Context()

	// Parse the XML body.
	var req DeleteObjectsRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	// S3 caps a batch delete at 1000 keys. Silently deleting more would leave a
	// client believing a request it should have had to split was accepted whole.
	if len(req.Objects) > maxDeleteObjects {
		renderAPIError(ctx, w, r, s3err.MalformedXML,
			errors.Errorf("%d keys exceeds the %d-key limit", len(req.Objects), maxDeleteObjects))

		return
	}

	result := DeleteObjectsResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}

	// Delete each object. Deleting a key that does not exist is a success in S3
	// (the operation is idempotent); any other failure is reported per-object
	// with its S3 error code.
	for _, obj := range req.Objects {
		cond, err := obj.conditions()
		if err == nil {
			err = h.deleteWithConditions(r, bucket, obj.Key, cond)
		}

		if err != nil && !errors.Is(err, fs.ErrObjectNotFound) {
			api := s3err.FromError(err)
			result.Errors = append(result.Errors, DeleteError{
				Key:     obj.Key,
				Code:    api.Code,
				Message: api.Message,
			})

			continue
		}

		if !req.Quiet {
			result.Deleted = append(result.Deleted, DeletedObject{Key: obj.Key})
		}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}
