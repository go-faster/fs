package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// Tagging is the XML document for object tagging (both request and response).
type Tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	TagSet  TagSet   `xml:"TagSet"`
}

// TagSet wraps the list of tags.
type TagSet struct {
	Tags []TagXML `xml:"Tag"`
}

// TagXML is a single tag entry.
type TagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// GetObjectTagging handles GET on an object with ?tagging.
func (h *handler) GetObjectTagging(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	tags, err := h.service.GetObjectTagging(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	resp := Tagging{
		Xmlns:  "http://s3.amazonaws.com/doc/2006-03-01/",
		TagSet: TagSet{Tags: make([]TagXML, len(tags))},
	}

	for i, tag := range tags {
		resp.TagSet.Tags[i] = TagXML(tag)
	}

	writeXML(ctx, w, r, resp)
}

// PutObjectTagging handles PUT on an object with ?tagging.
func (h *handler) PutObjectTagging(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	var doc Tagging
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	tags := make([]fs.Tag, len(doc.TagSet.Tags))
	for i, tag := range doc.TagSet.Tags {
		tags[i] = fs.Tag(tag)
	}

	if err := h.service.PutObjectTagging(ctx, bucket, key, tags); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteObjectTagging handles DELETE on an object with ?tagging.
func (h *handler) DeleteObjectTagging(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	if err := h.service.DeleteObjectTagging(ctx, bucket, key); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
