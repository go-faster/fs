package handler

import (
	"encoding/xml"
	"net/http"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// maxLifecycleRuleID is the longest rule ID S3 accepts.
const maxLifecycleRuleID = 255

// LifecycleConfigurationXML is the document the ?lifecycle subresource reads
// and writes.
type LifecycleConfigurationXML struct {
	XMLName xml.Name           `xml:"LifecycleConfiguration"`
	Xmlns   string             `xml:"xmlns,attr,omitempty"`
	Rules   []LifecycleRuleXML `xml:"Rule"`
}

// LifecycleRuleXML is one rule of that document.
//
// Every element the server does not enforce is declared here rather than left
// out. An undeclared element is silently discarded by encoding/xml, and a
// discarded lifecycle rule is a client told its data expires when nothing will
// ever delete it — so the unsupported ones are parsed precisely so they can be
// refused by name.
type LifecycleRuleXML struct {
	ID     string `xml:"ID,omitempty"`
	Status string `xml:"Status"`
	// Prefix is the pre-Filter spelling. S3 still accepts it and SDKs still
	// emit it, so both forms are read; Filter wins when a rule carries both.
	Prefix                         *string                 `xml:"Prefix"`
	Filter                         *LifecycleFilterXML     `xml:"Filter"`
	Expiration                     *LifecycleExpirationXML `xml:"Expiration"`
	AbortIncompleteMultipartUpload *LifecycleAbortXML      `xml:"AbortIncompleteMultipartUpload"`

	// Not enforced; present so PUT can refuse them.
	Transition                  []struct{} `xml:"Transition"`
	NoncurrentVersionExpiration *struct{}  `xml:"NoncurrentVersionExpiration"`
	NoncurrentVersionTransition []struct{} `xml:"NoncurrentVersionTransition"`
}

// LifecycleFilterXML is a rule's object filter.
type LifecycleFilterXML struct {
	Prefix *string `xml:"Prefix"`

	// Not enforced; present so PUT can refuse them.
	Tag                   *struct{} `xml:"Tag"`
	And                   *struct{} `xml:"And"`
	ObjectSizeGreaterThan *int64    `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64    `xml:"ObjectSizeLessThan"`
}

// LifecycleExpirationXML is a rule's object expiration.
type LifecycleExpirationXML struct {
	Days int    `xml:"Days,omitempty"`
	Date string `xml:"Date,omitempty"`

	// Not enforced; present so PUT can refuse it.
	ExpiredObjectDeleteMarker *bool `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

// LifecycleAbortXML is a rule's abandoned-upload cleanup.
type LifecycleAbortXML struct {
	DaysAfterInitiation int `xml:"DaysAfterInitiation"`
}

// GetBucketLifecycle serves GET ?lifecycle.
//
// A bucket with no configuration is NoSuchLifecycleConfiguration (404) rather
// than an empty document, the same distinction ?cors draws: clients branch on
// the error.
func (h *handler) GetBucketLifecycle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketLifecycleStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	rules, err := store.BucketLifecycle(ctx, bucket)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	if len(rules) == 0 {
		s3err.WriteAPI(w, r, s3err.NoSuchLifecycleConfiguration)
		return
	}

	doc := LifecycleConfigurationXML{Xmlns: s3XMLNamespace, Rules: make([]LifecycleRuleXML, 0, len(rules))}

	for _, rule := range rules {
		prefix := rule.Prefix
		out := LifecycleRuleXML{
			ID:     rule.ID,
			Status: rule.Status,
			Filter: &LifecycleFilterXML{Prefix: &prefix},
		}

		switch {
		case rule.ExpirationDays > 0:
			out.Expiration = &LifecycleExpirationXML{Days: rule.ExpirationDays}
		case !rule.ExpirationDate.IsZero():
			out.Expiration = &LifecycleExpirationXML{Date: rule.ExpirationDate.UTC().Format(lifecycleDateLayout)}
		}

		if days := rule.AbortIncompleteMultipartUploadDays; days > 0 {
			out.AbortIncompleteMultipartUpload = &LifecycleAbortXML{DaysAfterInitiation: days}
		}

		doc.Rules = append(doc.Rules, out)
	}

	writeXML(ctx, w, r, doc)
}

// PutBucketLifecycle serves PUT ?lifecycle.
func (h *handler) PutBucketLifecycle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketLifecycleStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	var doc LifecycleConfigurationXML
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	rules, apiErr, err := parseLifecycleRules(doc.Rules)
	if err != nil {
		renderAPIError(ctx, w, r, apiErr, err)
		return
	}

	if err := store.SetBucketLifecycle(ctx, bucket, rules); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteBucketLifecycle serves DELETE ?lifecycle.
func (h *handler) DeleteBucketLifecycle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketLifecycleStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	if err := store.DeleteBucketLifecycle(ctx, bucket); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// lifecycleDateLayout is the form S3 gives an expiration Date: midnight UTC,
// which is the only instant it accepts one at.
const lifecycleDateLayout = "2006-01-02T15:04:05Z"

// parseLifecycleRules converts the document's rules to the domain type,
// refusing anything the sweep does not enforce.
//
// The refusal is the point. A configuration is accepted whole or not at all: a
// rule kept minus the element that gave it meaning would delete objects the
// client never asked to lose, or leave alive the ones it did.
func parseLifecycleRules(in []LifecycleRuleXML) ([]fs.LifecycleRule, s3err.APIError, error) {
	if len(in) == 0 {
		return nil, s3err.MalformedXML, errors.New("no lifecycle rules")
	}

	rules := make([]fs.LifecycleRule, 0, len(in))
	seen := make(map[string]struct{}, len(in))

	for _, rule := range in {
		if unsupported := unsupportedLifecycleElement(rule); unsupported != "" {
			return nil, s3err.NotImplemented, errors.Errorf("lifecycle %s is not supported", unsupported)
		}

		if rule.Status != fs.LifecycleEnabled && rule.Status != fs.LifecycleDisabled {
			return nil, s3err.MalformedXML, errors.Errorf("invalid rule status %q", rule.Status)
		}

		if len(rule.ID) > maxLifecycleRuleID {
			return nil, s3err.InvalidArgument, errors.Errorf("rule ID longer than %d characters", maxLifecycleRuleID)
		}

		if _, dup := seen[rule.ID]; dup && rule.ID != "" {
			return nil, s3err.InvalidArgument, errors.Errorf("duplicate rule ID %q", rule.ID)
		}

		seen[rule.ID] = struct{}{}

		out := fs.LifecycleRule{ID: rule.ID, Status: rule.Status, Prefix: lifecyclePrefix(rule)}

		if exp := rule.Expiration; exp != nil {
			switch {
			case exp.Days > 0 && exp.Date != "":
				return nil, s3err.MalformedXML, errors.New("expiration takes Days or Date, not both")
			case exp.Days < 0:
				return nil, s3err.InvalidArgument, errors.New("expiration Days must be a positive integer")
			case exp.Days > 0:
				out.ExpirationDays = exp.Days
			case exp.Date != "":
				date, err := time.Parse(lifecycleDateLayout, exp.Date)
				if err != nil {
					return nil, s3err.MalformedXML, errors.Wrap(err, "parse expiration date")
				}

				out.ExpirationDate = date
			}
		}

		if abort := rule.AbortIncompleteMultipartUpload; abort != nil {
			if abort.DaysAfterInitiation <= 0 {
				return nil, s3err.InvalidArgument, errors.New("DaysAfterInitiation must be a positive integer")
			}

			out.AbortIncompleteMultipartUploadDays = abort.DaysAfterInitiation
		}

		if out.ExpirationDays == 0 && out.ExpirationDate.IsZero() && out.AbortIncompleteMultipartUploadDays == 0 {
			return nil, s3err.MalformedXML, errors.New("a rule must carry at least one action")
		}

		rules = append(rules, out)
	}

	return rules, s3err.APIError{}, nil
}

// lifecyclePrefix resolves the rule's prefix from either spelling; Filter wins
// when a rule carries both, which is what S3 documents.
func lifecyclePrefix(rule LifecycleRuleXML) string {
	if rule.Filter != nil && rule.Filter.Prefix != nil {
		return *rule.Filter.Prefix
	}

	if rule.Prefix != nil {
		return *rule.Prefix
	}

	return ""
}

// unsupportedLifecycleElement names the first element of rule this server does
// not enforce, or empty when the rule is entirely within the subset.
func unsupportedLifecycleElement(rule LifecycleRuleXML) string {
	switch {
	case len(rule.Transition) > 0:
		return "Transition"
	case rule.NoncurrentVersionExpiration != nil:
		return "NoncurrentVersionExpiration"
	case len(rule.NoncurrentVersionTransition) > 0:
		return "NoncurrentVersionTransition"
	case rule.Expiration != nil && rule.Expiration.ExpiredObjectDeleteMarker != nil:
		return "ExpiredObjectDeleteMarker"
	}

	if f := rule.Filter; f != nil {
		switch {
		case f.Tag != nil:
			return "Filter.Tag"
		case f.And != nil:
			return "Filter.And"
		case f.ObjectSizeGreaterThan != nil:
			return "Filter.ObjectSizeGreaterThan"
		case f.ObjectSizeLessThan != nil:
			return "Filter.ObjectSizeLessThan"
		}
	}

	return ""
}
