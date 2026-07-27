package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// VersioningConfigurationXML is the ?versioning document.
//
// Status is omitted for a bucket that has never been configured: S3 answers
// GetBucketVersioning on such a bucket with an empty configuration, and
// clients read the absence of the element — not a value — as "never
// versioned". Reporting "Suspended" there would be a different claim.
type VersioningConfigurationXML struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	Xmlns     string   `xml:"xmlns,attr,omitempty"`
	Status    string   `xml:"Status,omitempty"`
	MFADelete string   `xml:"MfaDelete,omitempty"`
}

// GetBucketVersioning serves GET ?versioning.
func (h *handler) GetBucketVersioning(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	versioner, ok := h.service.(fs.Versioner)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	state, err := versioner.BucketVersioning(ctx, bucket)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	writeXML(ctx, w, r, VersioningConfigurationXML{
		Xmlns:  s3XMLNamespace,
		Status: string(state),
	})
}

// PutBucketVersioning serves PUT ?versioning.
func (h *handler) PutBucketVersioning(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	versioner, ok := h.service.(fs.Versioner)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	var doc VersioningConfigurationXML
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	// MFA delete needs a second factor this server has no way to check.
	// Accepting the field and ignoring it would tell an operator their
	// versions are protected by something that is not there.
	if doc.MFADelete != "" && !strings.EqualFold(doc.MFADelete, "Disabled") {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	state := fs.VersioningState(doc.Status)

	switch state {
	case fs.VersioningEnabled, fs.VersioningSuspended:
	case fs.VersioningUnset:
		// There is no way back to never-versioned, so an empty status is not
		// a state to move to — it is a malformed request.
		renderAPIError(ctx, w, r, s3err.IllegalVersioningConfiguration,
			errors.New("versioning status is required"))

		return
	default:
		renderAPIError(ctx, w, r, s3err.IllegalVersioningConfiguration,
			errors.Errorf("unknown versioning status %q", doc.Status))

		return
	}

	if err := versioner.SetBucketVersioning(ctx, bucket, state); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}
