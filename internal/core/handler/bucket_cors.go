package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// CORSConfigurationXML is the document the ?cors subresource reads and writes.
type CORSConfigurationXML struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	Xmlns   string        `xml:"xmlns,attr,omitempty"`
	Rules   []CORSRuleXML `xml:"CORSRule"`
}

// CORSRuleXML is one rule of that document.
type CORSRuleXML struct {
	ID             string   `xml:"ID,omitempty"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedHeaders []string `xml:"AllowedHeader,omitempty"`
	ExposeHeaders  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds  int      `xml:"MaxAgeSeconds,omitempty"`
}

// GetBucketCORS serves GET ?cors.
//
// A bucket with no configuration is NoSuchCORSConfiguration (404), not an empty
// document: "I have no rules" and "I have a rule set that allows nothing" are
// different answers, and clients branch on the error.
func (h *handler) GetBucketCORS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketCORSStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	rules, err := store.BucketCORS(ctx, bucket)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	if len(rules) == 0 {
		s3err.WriteAPI(w, r, s3err.NoSuchCORSConfiguration)
		return
	}

	doc := CORSConfigurationXML{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Rules: make([]CORSRuleXML, 0, len(rules))}
	for _, rule := range rules {
		doc.Rules = append(doc.Rules, CORSRuleXML{
			AllowedOrigins: rule.AllowedOrigins,
			AllowedMethods: rule.AllowedMethods,
			AllowedHeaders: rule.AllowedHeaders,
			ExposeHeaders:  rule.ExposeHeaders,
			MaxAgeSeconds:  rule.MaxAgeSeconds,
		})
	}

	writeXML(ctx, w, r, doc)
}

// PutBucketCORS serves PUT ?cors.
func (h *handler) PutBucketCORS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketCORSStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	var doc CORSConfigurationXML
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	if len(doc.Rules) == 0 {
		renderAPIError(ctx, w, r, s3err.MalformedXML, errors.New("no CORS rules"))
		return
	}

	rules := make([]fs.CORSRule, 0, len(doc.Rules))

	for _, rule := range doc.Rules {
		if len(rule.AllowedOrigins) == 0 || len(rule.AllowedMethods) == 0 {
			renderAPIError(ctx, w, r, s3err.MalformedXML,
				errors.New("a CORS rule needs an origin and a method"))

			return
		}

		for _, method := range rule.AllowedMethods {
			if !allowedCORSMethod(method) {
				renderAPIError(ctx, w, r, s3err.InvalidRequest,
					errors.Errorf("unsupported CORS method %q", method))

				return
			}
		}

		rules = append(rules, fs.CORSRule{
			AllowedOrigins: rule.AllowedOrigins,
			AllowedMethods: rule.AllowedMethods,
			AllowedHeaders: rule.AllowedHeaders,
			ExposeHeaders:  rule.ExposeHeaders,
			MaxAgeSeconds:  rule.MaxAgeSeconds,
		})
	}

	if err := store.SetBucketCORS(ctx, bucket, rules); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteBucketCORS serves DELETE ?cors.
func (h *handler) DeleteBucketCORS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	store, ok := h.service.(fs.BucketCORSStore)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	if err := store.DeleteBucketCORS(ctx, bucket); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// corsMethods are the methods a CORS rule may allow. S3 rejects anything else,
// including methods it implements but does not expose to browsers.
var corsMethods = []string{
	http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodHead,
}

func allowedCORSMethod(method string) bool {
	for _, m := range corsMethods {
		if m == method {
			return true
		}
	}

	return false
}
