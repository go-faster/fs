package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// PublicAccessBlockXML is the ?publicAccessBlock document.
type PublicAccessBlockXML struct {
	XMLName               xml.Name `xml:"PublicAccessBlockConfiguration"`
	Xmlns                 string   `xml:"xmlns,attr,omitempty"`
	BlockPublicAcls       bool     `xml:"BlockPublicAcls"`
	IgnorePublicAcls      bool     `xml:"IgnorePublicAcls"`
	BlockPublicPolicy     bool     `xml:"BlockPublicPolicy"`
	RestrictPublicBuckets bool     `xml:"RestrictPublicBuckets"`
}

// OwnershipControlsXML is the ?ownershipControls document.
type OwnershipControlsXML struct {
	XMLName xml.Name           `xml:"OwnershipControls"`
	Xmlns   string             `xml:"xmlns,attr,omitempty"`
	Rules   []OwnershipRuleXML `xml:"Rule"`
}

// OwnershipRuleXML is one rule of that document. S3 permits exactly one.
type OwnershipRuleXML struct {
	ObjectOwnership string `xml:"ObjectOwnership"`
}

// GetBucketPublicAccessBlock serves GET ?publicAccessBlock.
func (h *handler) GetBucketPublicAccessBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketSettingsStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	block, err := store.BucketPublicAccessBlock(ctx, bucket)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	// An absent configuration is its own answer, not four false switches: a
	// caller has to be able to tell "nobody has said anything" from "everything
	// is explicitly allowed".
	if block == nil {
		s3err.WriteAPI(w, r, s3err.NoSuchPublicAccessBlock)
		return
	}

	writeXML(ctx, w, r, PublicAccessBlockXML{
		Xmlns:                 s3XMLNamespace,
		BlockPublicAcls:       block.BlockPublicACLs,
		IgnorePublicAcls:      block.IgnorePublicACLs,
		BlockPublicPolicy:     block.BlockPublicPolicy,
		RestrictPublicBuckets: block.RestrictPublicBuckets,
	})
}

// PutBucketPublicAccessBlock serves PUT ?publicAccessBlock.
func (h *handler) PutBucketPublicAccessBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketSettingsStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	var doc PublicAccessBlockXML
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	block := &fs.PublicAccessBlock{
		BlockPublicACLs:       doc.BlockPublicAcls,
		IgnorePublicACLs:      doc.IgnorePublicAcls,
		BlockPublicPolicy:     doc.BlockPublicPolicy,
		RestrictPublicBuckets: doc.RestrictPublicBuckets,
	}

	if err := store.SetBucketPublicAccessBlock(ctx, bucket, block); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteBucketPublicAccessBlock serves DELETE ?publicAccessBlock. Deleting a
// configuration that is not there succeeds, as every S3 delete does.
func (h *handler) DeleteBucketPublicAccessBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketSettingsStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	if err := store.SetBucketPublicAccessBlock(ctx, bucket, nil); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// GetBucketOwnershipControls serves GET ?ownershipControls.
func (h *handler) GetBucketOwnershipControls(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketSettingsStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	ownership, err := store.BucketObjectOwnership(ctx, bucket)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	if ownership == "" {
		s3err.WriteAPI(w, r, s3err.OwnershipControlsNotFound)
		return
	}

	writeXML(ctx, w, r, OwnershipControlsXML{
		Xmlns: s3XMLNamespace,
		Rules: []OwnershipRuleXML{{ObjectOwnership: ownership}},
	})
}

// PutBucketOwnershipControls serves PUT ?ownershipControls.
func (h *handler) PutBucketOwnershipControls(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketSettingsStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	var doc OwnershipControlsXML
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	if len(doc.Rules) != 1 {
		renderAPIError(ctx, w, r, s3err.MalformedXML,
			errors.Errorf("expected exactly one rule, got %d", len(doc.Rules)))

		return
	}

	ownership := doc.Rules[0].ObjectOwnership
	if !validObjectOwnership(ownership) {
		renderAPIError(ctx, w, r, s3err.InvalidArgument,
			errors.Errorf("unknown object ownership %q", ownership))

		return
	}

	if err := store.SetBucketObjectOwnership(ctx, bucket, ownership); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteBucketOwnershipControls serves DELETE ?ownershipControls, and succeeds
// when there is nothing to delete.
func (h *handler) DeleteBucketOwnershipControls(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketSettingsStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	if err := store.SetBucketObjectOwnership(ctx, bucket, ""); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// validObjectOwnership reports whether the value names an ownership rule S3
// defines. An unknown one is rejected rather than stored: a client that
// mistypes it must not be told its bucket is configured.
func validObjectOwnership(ownership string) bool {
	switch ownership {
	case fs.OwnershipBucketOwnerEnforced,
		fs.OwnershipBucketOwnerPreferred,
		fs.OwnershipObjectWriter:
		return true
	default:
		return false
	}
}
