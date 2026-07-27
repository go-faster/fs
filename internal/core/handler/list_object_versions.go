package handler

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// VersionEntry is a single object version in a ListObjectVersions response.
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
	DeleteMarkers  []VersionEntry `xml:"DeleteMarker"`
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
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	q := r.URL.Query()
	encodeURL := q.Get("encoding-type") == encodingTypeURL

	maxKeys := defaultMaxKeys
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < defaultMaxKeys {
			maxKeys = n
		}
	}

	page, err := h.listVersions(ctx, &fs.ListObjectVersionsRequest{
		Bucket:          bucket,
		Prefix:          q.Get("prefix"),
		Delimiter:       q.Get("delimiter"),
		KeyMarker:       q.Get("key-marker"),
		VersionIDMarker: q.Get("version-id-marker"),
		Limit:           maxKeys,
	})
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	maybeEncode := func(s string) string {
		if encodeURL {
			return url.QueryEscape(s)
		}

		return s
	}

	resp := ListVersionsResult{
		Name:            bucket,
		Prefix:          maybeEncode(q.Get("prefix")),
		KeyMarker:       maybeEncode(q.Get("key-marker")),
		VersionIDMarker: q.Get("version-id-marker"),
		MaxKeys:         maxKeys,
		Delimiter:       maybeEncode(q.Get("delimiter")),
		IsTruncated:     page.IsTruncated,
	}

	// Versions and delete markers are reported in separate elements even
	// though they are one ordered sequence: a marker has no content, so the
	// shapes differ, and a client that only cares about content reads one
	// element and ignores the other.
	for _, v := range page.Versions {
		entry := VersionEntry{
			Key:          maybeEncode(v.Key),
			VersionID:    v.VersionID,
			IsLatest:     v.IsLatest,
			LastModified: v.LastModified,
			ETag:         quoteETag(v.ETag),
			Size:         v.Size,
		}

		if v.DeleteMarker {
			entry.ETag, entry.Size = "", 0
			resp.DeleteMarkers = append(resp.DeleteMarkers, entry)

			continue
		}

		resp.Versions = append(resp.Versions, entry)
	}

	for _, cp := range page.CommonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, CommonPrefix{Prefix: maybeEncode(cp)})
	}

	if encodeURL {
		resp.EncodingType = encodingTypeURL
	}

	if page.IsTruncated {
		resp.NextKeyMarker = maybeEncode(page.NextKeyMarker)
		resp.NextVersionIDMarker = page.NextVersionIDMarker
	}

	writeXML(ctx, w, r, resp)
}

// listVersions returns a page of versions, falling back to the object listing
// for a backend that does not keep versions.
//
// That fallback is not a stub: for a bucket that was never versioned, every
// object *is* its own "null" version, and that is exactly what S3 reports. A
// backend that cannot version has only such buckets, so the answer is right
// rather than merely non-empty.
//
// The fallback turns on what the service *answers*, not on whether it
// type-asserts: the service implements fs.Versioner unconditionally and
// resolves the backend's capability underneath, so the assertion always
// succeeds and only ErrUnsupportedOperation distinguishes a backend that
// cannot version. Keying off the assertion alone left every such backend —
// clusterstore among them — answering NotImplemented to a listing it can
// perfectly well serve.
func (h *handler) listVersions(
	ctx context.Context, req *fs.ListObjectVersionsRequest,
) (*fs.ListObjectVersionsResponse, error) {
	if versioner, ok := h.service.(fs.Versioner); ok {
		switch resp, err := versioner.ListObjectVersions(ctx, req); {
		case err == nil:
			return resp, nil
		case !errors.Is(err, fs.ErrUnsupportedOperation):
			return nil, err
		}
	}

	page, err := h.service.ListObjects(ctx, &fs.ListObjectsRequest{
		Bucket:     req.Bucket,
		Prefix:     req.Prefix,
		Delimiter:  req.Delimiter,
		StartAfter: req.KeyMarker,
		Limit:      req.Limit,
	})
	if err != nil {
		return nil, err
	}

	out := &fs.ListObjectVersionsResponse{
		CommonPrefixes:      page.CommonPrefixes,
		IsTruncated:         page.IsTruncated,
		NextKeyMarker:       page.NextStartAfter,
		NextVersionIDMarker: fs.NullVersionID,
	}

	for _, o := range page.Objects {
		out.Versions = append(out.Versions, fs.ObjectVersion{
			Key:          o.Key,
			VersionID:    fs.NullVersionID,
			IsLatest:     true,
			Size:         o.Size,
			ETag:         o.ETag,
			LastModified: o.LastModified,
			Owner:        o.Owner,
		})
	}

	return out, nil
}
