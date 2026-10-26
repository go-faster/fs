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
	Prefix                         *string                           `xml:"Prefix"`
	Filter                         *LifecycleFilterXML               `xml:"Filter"`
	Expiration                     *LifecycleExpirationXML           `xml:"Expiration"`
	NoncurrentVersionExpiration    *LifecycleNoncurrentExpirationXML `xml:"NoncurrentVersionExpiration"`
	AbortIncompleteMultipartUpload *LifecycleAbortXML                `xml:"AbortIncompleteMultipartUpload"`

	// Not enforced; present so PUT can refuse them.
	Transition                  []struct{} `xml:"Transition"`
	NoncurrentVersionTransition []struct{} `xml:"NoncurrentVersionTransition"`
}

// LifecycleFilterXML is a rule's object filter. S3 allows exactly one of its
// elements; several predicates combine inside And.
type LifecycleFilterXML struct {
	Prefix                *string                `xml:"Prefix"`
	Tag                   *LifecycleTagXML       `xml:"Tag"`
	ObjectSizeGreaterThan *int64                 `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64                 `xml:"ObjectSizeLessThan"`
	And                   *LifecycleFilterAndXML `xml:"And"`
}

// LifecycleFilterAndXML is a filter's conjunction: an object matches when
// every predicate holds.
type LifecycleFilterAndXML struct {
	Prefix                *string           `xml:"Prefix"`
	Tags                  []LifecycleTagXML `xml:"Tag"`
	ObjectSizeGreaterThan *int64            `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64            `xml:"ObjectSizeLessThan"`
}

// LifecycleTagXML is a tag a filter requires.
type LifecycleTagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// LifecycleExpirationXML is a rule's object expiration.
type LifecycleExpirationXML struct {
	Days                      int    `xml:"Days,omitempty"`
	Date                      string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker *bool  `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

// LifecycleNoncurrentExpirationXML is a rule's removal of noncurrent
// versions. Both elements are pointers, so an absent one is told from a zero.
type LifecycleNoncurrentExpirationXML struct {
	NoncurrentDays          *int `xml:"NoncurrentDays"`
	NewerNoncurrentVersions *int `xml:"NewerNoncurrentVersions,omitempty"`
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
		out := LifecycleRuleXML{
			ID:     rule.ID,
			Status: rule.Status,
			Filter: lifecycleFilter(rule),
		}

		switch {
		case rule.ExpirationDays > 0:
			out.Expiration = &LifecycleExpirationXML{Days: rule.ExpirationDays}
		case !rule.ExpirationDate.IsZero():
			out.Expiration = &LifecycleExpirationXML{Date: rule.ExpirationDate.UTC().Format(lifecycleDateLayout)}
		case rule.ExpiredObjectDeleteMarker:
			marker := true
			out.Expiration = &LifecycleExpirationXML{ExpiredObjectDeleteMarker: &marker}
		}

		if days := rule.NoncurrentDays; days > 0 {
			nve := &LifecycleNoncurrentExpirationXML{NoncurrentDays: &days}
			if newer := rule.NewerNoncurrentVersions; newer > 0 {
				nve.NewerNoncurrentVersions = &newer
			}

			out.NoncurrentVersionExpiration = nve
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

		out := fs.LifecycleRule{ID: rule.ID, Status: rule.Status}

		if apiErr, err := parseLifecycleFilter(rule, &out); err != nil {
			return nil, apiErr, err
		}

		if exp := rule.Expiration; exp != nil {
			marker := exp.ExpiredObjectDeleteMarker != nil

			switch {
			case exp.Days > 0 && exp.Date != "":
				return nil, s3err.MalformedXML, errors.New("expiration takes Days or Date, not both")
			case marker && (exp.Days > 0 || exp.Date != ""):
				// AWS names no error for this; Ceph RGW answers MalformedXML.
				return nil, s3err.MalformedXML, errors.New("expiration takes ExpiredObjectDeleteMarker or Days or Date, not two")
			case marker:
				out.ExpiredObjectDeleteMarker = *exp.ExpiredObjectDeleteMarker
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

		if nve := rule.NoncurrentVersionExpiration; nve != nil {
			apiErr, err := parseNoncurrentExpiration(rule, nve, &out)
			if err != nil {
				return nil, apiErr, err
			}
		}

		if out.ExpirationDays == 0 && out.ExpirationDate.IsZero() && out.AbortIncompleteMultipartUploadDays == 0 &&
			out.NoncurrentDays == 0 && !out.ExpiredObjectDeleteMarker {
			return nil, s3err.MalformedXML, errors.New("a rule must carry at least one action")
		}

		// Delete markers and uploads in progress have no tags, and uploads no
		// size yet: such a rule could never act, and is refused rather than
		// kept. AWS documents the tag cases; Ceph RGW answers InvalidRequest.
		switch {
		case out.HasTags() && out.ExpiredObjectDeleteMarker:
			return nil, s3err.InvalidRequest, errors.New("ExpiredObjectDeleteMarker cannot be used with a tag filter")
		case out.HasTags() && out.AbortIncompleteMultipartUploadDays > 0:
			return nil, s3err.InvalidRequest, errors.New("AbortIncompleteMultipartUpload cannot be used with a tag filter")
		case out.HasSize() && out.AbortIncompleteMultipartUploadDays > 0:
			return nil, s3err.InvalidRequest, errors.New("AbortIncompleteMultipartUpload cannot be used with a size filter")
		}

		rules = append(rules, out)
	}

	return rules, s3err.APIError{}, nil
}

// maxNewerNoncurrentVersions is the most noncurrent versions a rule may keep.
const maxNewerNoncurrentVersions = 100

// parseNoncurrentExpiration validates a rule's NoncurrentVersionExpiration into
// out.
func parseNoncurrentExpiration(rule LifecycleRuleXML, nve *LifecycleNoncurrentExpirationXML, out *fs.LifecycleRule) (s3err.APIError, error) {
	// NewerNoncurrentVersions only narrows NoncurrentDays ("both values must
	// be exceeded"), so a rule without the days is malformed, as Ceph RGW
	// answers.
	if nve.NoncurrentDays == nil {
		return s3err.MalformedXML, errors.New("NoncurrentVersionExpiration needs NoncurrentDays")
	}

	if *nve.NoncurrentDays <= 0 {
		return s3err.InvalidArgument, errors.New("NoncurrentDays must be a positive integer")
	}

	out.NoncurrentDays = *nve.NoncurrentDays

	if newer := nve.NewerNoncurrentVersions; newer != nil {
		if *newer < 1 || *newer > maxNewerNoncurrentVersions {
			return s3err.InvalidArgument, errors.Errorf("NewerNoncurrentVersions must be between 1 and %d", maxNewerNoncurrentVersions)
		}

		// AWS documents this refusal; the legacy rule-level Prefix does not
		// count as a filter.
		if rule.Filter == nil {
			return s3err.InvalidRequest, errors.New("NewerNoncurrentVersions needs a Filter element")
		}

		out.NewerNoncurrentVersions = *newer
	}

	return s3err.APIError{}, nil
}

// parseLifecycleFilter resolves the rule's filter into out. The legacy
// rule-level Prefix is read when there is no Filter; Filter wins when a rule
// carries both, which is what S3 documents.
func parseLifecycleFilter(rule LifecycleRuleXML, out *fs.LifecycleRule) (s3err.APIError, error) {
	f := rule.Filter
	if f == nil {
		if rule.Prefix != nil {
			out.Prefix = *rule.Prefix
		}

		return s3err.APIError{}, nil
	}

	predicates := 0

	for _, set := range []bool{f.Prefix != nil, f.Tag != nil, f.ObjectSizeGreaterThan != nil, f.ObjectSizeLessThan != nil, f.And != nil} {
		if set {
			predicates++
		}
	}

	// S3 documents exactly one; Ceph RGW accepts several and ANDs them. An
	// SDK sends whatever its caller built, so the refusal is what tells a
	// client its filter needs an And.
	if predicates > 1 {
		return s3err.MalformedXML, errors.New("a Filter takes one predicate; combine several in And")
	}

	gt, lt := f.ObjectSizeGreaterThan, f.ObjectSizeLessThan

	switch {
	case f.Prefix != nil:
		out.Prefix = *f.Prefix
	case f.Tag != nil:
		out.Tags = []fs.Tag{{Key: f.Tag.Key, Value: f.Tag.Value}}
	case f.And != nil:
		if f.And.Prefix != nil {
			out.Prefix = *f.And.Prefix
		}

		for _, tag := range f.And.Tags {
			out.Tags = append(out.Tags, fs.Tag{Key: tag.Key, Value: tag.Value})
		}

		gt, lt = f.And.ObjectSizeGreaterThan, f.And.ObjectSizeLessThan
	}

	if (gt != nil && *gt < 0) || (lt != nil && *lt < 0) {
		return s3err.InvalidArgument, errors.New("object size bounds must not be negative")
	}

	// Ceph RGW answers MalformedXML; AWS documents the rule without a code.
	if gt != nil && lt != nil && *gt >= *lt {
		return s3err.MalformedXML, errors.New("ObjectSizeGreaterThan must be less than ObjectSizeLessThan")
	}

	out.SizeGreaterThan, out.SizeLessThan = gt, lt

	return s3err.APIError{}, nil
}

// lifecycleFilter renders a rule's filter as S3 does: a single predicate
// bare, several inside And. An empty filter is an empty prefix, the whole
// bucket.
func lifecycleFilter(rule fs.LifecycleRule) *LifecycleFilterXML {
	predicates := len(rule.Tags)

	for _, set := range []bool{rule.Prefix != "", rule.SizeGreaterThan != nil, rule.SizeLessThan != nil} {
		if set {
			predicates++
		}
	}

	if predicates > 1 {
		and := &LifecycleFilterAndXML{
			ObjectSizeGreaterThan: rule.SizeGreaterThan,
			ObjectSizeLessThan:    rule.SizeLessThan,
		}
		if rule.Prefix != "" {
			and.Prefix = &rule.Prefix
		}

		for _, tag := range rule.Tags {
			and.Tags = append(and.Tags, LifecycleTagXML{Key: tag.Key, Value: tag.Value})
		}

		return &LifecycleFilterXML{And: and}
	}

	switch {
	case len(rule.Tags) == 1:
		return &LifecycleFilterXML{Tag: &LifecycleTagXML{Key: rule.Tags[0].Key, Value: rule.Tags[0].Value}}
	case rule.SizeGreaterThan != nil:
		return &LifecycleFilterXML{ObjectSizeGreaterThan: rule.SizeGreaterThan}
	case rule.SizeLessThan != nil:
		return &LifecycleFilterXML{ObjectSizeLessThan: rule.SizeLessThan}
	default:
		prefix := rule.Prefix

		return &LifecycleFilterXML{Prefix: &prefix}
	}
}

// unsupportedLifecycleElement names the first element of rule this server does
// not enforce, or empty when the rule is entirely within the subset.
func unsupportedLifecycleElement(rule LifecycleRuleXML) string {
	switch {
	case len(rule.Transition) > 0:
		return "Transition"
	case len(rule.NoncurrentVersionTransition) > 0:
		return "NoncurrentVersionTransition"
	}

	return ""
}
