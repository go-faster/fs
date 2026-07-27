package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
	"github.com/go-faster/fs/internal/sse"
)

// ServerSideEncryptionConfigurationXML is the ?encryption document.
type ServerSideEncryptionConfigurationXML struct {
	XMLName xml.Name            `xml:"ServerSideEncryptionConfiguration"`
	Xmlns   string              `xml:"xmlns,attr,omitempty"`
	Rules   []EncryptionRuleXML `xml:"Rule"`
}

// EncryptionRuleXML is one rule of an ?encryption document.
type EncryptionRuleXML struct {
	Default EncryptionDefaultXML `xml:"ApplyServerSideEncryptionByDefault"`
	// BucketKeyEnabled is an SSE-KMS cost optimization. It is parsed so a
	// document containing it is not malformed, and ignored because it means
	// nothing without KMS.
	BucketKeyEnabled bool `xml:"BucketKeyEnabled,omitempty"`
}

// EncryptionDefaultXML names the algorithm a rule applies.
type EncryptionDefaultXML struct {
	SSEAlgorithm   string `xml:"SSEAlgorithm"`
	KMSMasterKeyID string `xml:"KMSMasterKeyID,omitempty"`
}

// GetBucketEncryption serves GET ?encryption.
func (h *handler) GetBucketEncryption(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	// Two different things can mean "no default encryption here", and both
	// end as NotImplemented. The handler may be wired straight to a backend
	// that does not implement the capability at all, which is what this
	// assertion catches; or to the service, which implements it
	// unconditionally and answers ErrUnsupportedOperation when the backend
	// underneath cannot encrypt.
	store, ok := h.service.(fs.BucketEncrypter)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	algorithm, err := store.BucketEncryption(ctx, bucket)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	if algorithm == "" {
		s3err.WriteAPI(w, r, s3err.ServerSideEncryptionConfigurationNotFound)
		return
	}

	writeXML(ctx, w, r, ServerSideEncryptionConfigurationXML{
		Xmlns: s3XMLNamespace,
		Rules: []EncryptionRuleXML{{
			Default: EncryptionDefaultXML{SSEAlgorithm: algorithm},
		}},
	})
}

// PutBucketEncryption serves PUT ?encryption.
func (h *handler) PutBucketEncryption(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketEncrypter)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	var doc ServerSideEncryptionConfigurationXML
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	if len(doc.Rules) != 1 {
		renderAPIError(ctx, w, r, s3err.MalformedXML,
			errors.Errorf("expected exactly one rule, got %d", len(doc.Rules)))

		return
	}

	algorithm := doc.Rules[0].Default.SSEAlgorithm

	// An algorithm this server cannot perform is refused rather than stored.
	// Storing it would leave the bucket configured for an encryption that
	// never happens, and every object written to it would look protected.
	if algorithm != sse.Algorithm {
		renderAPIError(ctx, w, r, s3err.InvalidArgument,
			errors.Errorf("unsupported SSEAlgorithm %q, want %q", algorithm, sse.Algorithm))

		return
	}

	if err := store.SetBucketEncryption(ctx, bucket, algorithm); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteBucketEncryption serves DELETE ?encryption, and succeeds when there is
// nothing to delete.
func (h *handler) DeleteBucketEncryption(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketEncrypter)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	if err := store.SetBucketEncryption(ctx, bucket, ""); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// bucketDefaultEncryption returns the bucket's configured default, and empty
// when it has none or the backend cannot remember one.
//
// A failure to read it is reported as no default rather than as an error: this
// runs on the write path, and the alternative is failing a PUT because a
// setting could not be consulted.
func (h *handler) bucketDefaultEncryption(r *http.Request, bucket string) string {
	store, ok := h.service.(fs.BucketEncrypter)
	if !ok {
		return ""
	}

	algorithm, err := store.BucketEncryption(r.Context(), bucket)
	if err != nil {
		return ""
	}

	return algorithm
}
