package handler

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// userMetadataPrefix is the header prefix for user-defined object metadata.
const userMetadataPrefix = "X-Amz-Meta-"

// extractObjectMetadata collects the representation headers and x-amz-meta-*
// pairs from a request into the domain metadata type.
func extractObjectMetadata(header http.Header) fs.ObjectMetadata {
	meta := fs.ObjectMetadata{
		ContentType:        header.Get("Content-Type"),
		CacheControl:       header.Get("Cache-Control"),
		ContentDisposition: header.Get("Content-Disposition"),
		ContentEncoding:    cleanContentEncoding(header.Get("Content-Encoding")),
	}

	for name, values := range header {
		if !strings.HasPrefix(name, userMetadataPrefix) || len(values) == 0 {
			continue
		}

		key := strings.ToLower(strings.TrimPrefix(name, userMetadataPrefix))
		if key == "" {
			continue
		}

		if meta.UserMetadata == nil {
			meta.UserMetadata = make(map[string]string)
		}

		meta.UserMetadata[key] = values[0]
	}

	return meta
}

// cleanContentEncoding drops the transport-only "aws-chunked" token from a
// Content-Encoding value, keeping any real encodings (e.g. gzip).
func cleanContentEncoding(value string) string {
	if value == "" {
		return ""
	}

	var kept []string

	for tok := range strings.SplitSeq(value, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" || strings.EqualFold(tok, "aws-chunked") {
			continue
		}

		kept = append(kept, tok)
	}

	return strings.Join(kept, ",")
}

// writeObjectMetadata emits the stored metadata as response headers.
func writeObjectMetadata(h http.Header, meta fs.ObjectMetadata) {
	if meta.ContentType != "" {
		h.Set("Content-Type", meta.ContentType)
	}

	if meta.CacheControl != "" {
		h.Set("Cache-Control", meta.CacheControl)
	}

	if meta.ContentDisposition != "" {
		h.Set("Content-Disposition", meta.ContentDisposition)
	}

	if meta.ContentEncoding != "" {
		h.Set("Content-Encoding", meta.ContentEncoding)
	}

	// Deterministic emission order for tests and logs.
	keys := make([]string, 0, len(meta.UserMetadata))
	for k := range meta.UserMetadata {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		// Assign directly to keep the all-lowercase header name AWS emits
		// (h.Set would canonicalize x-amz-meta-color to X-Amz-Meta-Color, and
		// SDKs surface the key casing verbatim).
		h[strings.ToLower(userMetadataPrefix)+k] = []string{meta.UserMetadata[k]}
	}
}

// parseTaggingHeader parses an x-amz-tagging header (URL-encoded query format,
// e.g. "k1=v1&k2=v2") into a tag list, preserving order.
func parseTaggingHeader(value string) ([]fs.Tag, error) {
	if value == "" {
		return nil, nil
	}

	var tags []fs.Tag

	for pair := range strings.SplitSeq(value, "&") {
		if pair == "" {
			continue
		}

		rawKey, rawValue, _ := strings.Cut(pair, "=")

		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return nil, errors.Wrap(err, "tag key")
		}

		val, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, errors.Wrap(err, "tag value")
		}

		tags = append(tags, fs.Tag{Key: key, Value: val})
	}

	return tags, nil
}
