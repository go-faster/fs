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
		Expires:            header.Get("Expires"),
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

		meta.UserMetadata[key] = decodeHeaderValue(values[0])
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

	// ", " and not ",": the client gets back the list it sent, and a client
	// that compares the header verbatim (they do) sees no difference.
	return strings.Join(kept, ", ")
}

// decodeHeaderValue interprets a raw header value as UTF-8 text.
//
// Go hands header bytes over untouched, so a value carrying multi-byte UTF-8
// arrives as a Go string of those same bytes. Keeping it that way is right:
// the text is already what the client meant. What matters is that
// writeObjectMetadata puts it back on the wire the way HTTP header values are
// read, which is one byte per character — see encodeHeaderValue.
func decodeHeaderValue(v string) string {
	return v
}

// encodeHeaderValue renders a metadata value for the wire.
//
// HTTP header values are bytes, and every client decodes them one byte per
// character (ISO-8859-1) — that is what the RFC says and what urllib3, .NET
// and the AWS SDKs do. Echoing stored UTF-8 bytes back therefore shows the
// client mojibake: it sent "é" and reads back "Ã©". Latin-1-encoding the
// value undoes that for every character that fits, which is the whole of
// Latin-1 and so the whole of what a byte-oriented client could have sent.
//
// Text outside Latin-1 has no such encoding; it goes out as UTF-8 bytes,
// unchanged from what was stored, because mangling it further would help
// nobody.
func encodeHeaderValue(v string) string {
	if isASCII(v) {
		return v
	}

	out := make([]byte, 0, len(v))

	for _, r := range v {
		if r > 0xFF {
			return v
		}

		out = append(out, byte(r))
	}

	return string(out)
}

// isASCII reports whether s is plain ASCII, the common case that needs no
// re-encoding at all.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}

	return true
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

	if meta.Expires != "" {
		h.Set("Expires", meta.Expires)
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
		h[strings.ToLower(userMetadataPrefix)+k] = []string{encodeHeaderValue(meta.UserMetadata[k])}
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
