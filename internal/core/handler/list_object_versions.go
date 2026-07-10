package handler

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// VersionEntry is a single object version in a ListObjectVersions response.
//
// The store is unversioned, so every object is reported as a single current
// version with the well-known VersionId "null" and IsLatest=true — the shape
// AWS itself returns for objects in a never-versioned bucket.
type VersionEntry struct {
	Key          string    `xml:"Key"`
	VersionID    string    `xml:"VersionId"`
	IsLatest     bool      `xml:"IsLatest"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag,omitempty"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass,omitempty"`
}

// ListVersionsResult is the XML response for ListObjectVersions
// (GET /{bucket}?versions).
type ListVersionsResult struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListVersionsResult"`
	Name    string   `xml:"Name"`
	Prefix  string   `xml:"Prefix"`

	KeyMarker           string `xml:"KeyMarker"`
	VersionIDMarker     string `xml:"VersionIdMarker"`
	NextKeyMarker       string `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string `xml:"NextVersionIdMarker,omitempty"`

	MaxKeys      int    `xml:"MaxKeys"`
	Delimiter    string `xml:"Delimiter,omitempty"`
	EncodingType string `xml:"EncodingType,omitempty"`
	IsTruncated  bool   `xml:"IsTruncated"`

	Versions       []VersionEntry `xml:"Version"`
	CommonPrefixes []CommonPrefix `xml:"CommonPrefixes"`
}

// unversionedVersionID is the version identifier reported for objects in a
// store without versioning, matching AWS's behavior for never-versioned
// buckets.
const unversionedVersionID = "null"

// ListObjectVersions implements GET /{bucket}?versions. On an unversioned store
// it lists current objects as single "null" versions. It exists chiefly so S3
// clients and tooling that enumerate objects for deletion via
// list_object_versions (rather than list_objects) work correctly.
func (h *handler) ListObjectVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")

	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	encodeURL := q.Get("encoding-type") == encodingTypeURL
	keyMarker := q.Get("key-marker")

	maxKeys := defaultMaxKeys
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < defaultMaxKeys {
			maxKeys = n
		}
	}

	objects, err := h.service.ListObjects(ctx, bucket, prefix)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	entries := buildListEntries(objects, prefix, delimiter)

	maybeEncode := func(s string) string {
		if encodeURL {
			return url.QueryEscape(s)
		}

		return s
	}

	var (
		versions       []VersionEntry
		commonPrefixes []CommonPrefix
		count          int
		truncated      bool
		nextKeyMarker  string
	)

	for _, e := range entries {
		if keyMarker != "" && e.key <= keyMarker {
			continue
		}

		if count >= maxKeys {
			truncated = true
			break
		}

		if e.isPrefix {
			commonPrefixes = append(commonPrefixes, CommonPrefix{Prefix: maybeEncode(e.key)})
		} else {
			versions = append(versions, VersionEntry{
				Key:          maybeEncode(e.obj.Key),
				VersionID:    unversionedVersionID,
				IsLatest:     true,
				LastModified: e.obj.LastModified,
				ETag:         quoteETag(e.obj.ETag),
				Size:         e.obj.Size,
			})
		}

		nextKeyMarker = e.key
		count++
	}

	resp := ListVersionsResult{
		Name:           bucket,
		Prefix:         maybeEncode(prefix),
		KeyMarker:      maybeEncode(keyMarker),
		MaxKeys:        maxKeys,
		Delimiter:      maybeEncode(delimiter),
		IsTruncated:    truncated,
		Versions:       versions,
		CommonPrefixes: commonPrefixes,
	}

	if encodeURL {
		resp.EncodingType = encodingTypeURL
	}

	if truncated {
		resp.NextKeyMarker = maybeEncode(nextKeyMarker)
	}

	writeXML(ctx, w, r, resp)
}
