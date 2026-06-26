package handler

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// defaultMaxKeys is the S3 default and maximum number of keys returned per page.
const defaultMaxKeys = 1000

// listEntry is a single item in the ordered listing keyspace: either an object or
// a delimiter-derived common prefix.
type listEntry struct {
	key      string
	obj      fs.Object
	isPrefix bool
}

func (h *handler) ListObjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")

	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	encodeURL := q.Get("encoding-type") == "url"
	isV2 := q.Get("list-type") == "2"

	maxKeys := defaultMaxKeys
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < defaultMaxKeys {
			maxKeys = n
		}
	}

	// cursor is the exclusive lower bound for pagination.
	var cursor string

	switch {
	case isV2 && q.Get("continuation-token") != "":
		cursor = decodeContinuationToken(q.Get("continuation-token"))
	case isV2:
		cursor = q.Get("start-after")
	default:
		cursor = q.Get("marker")
	}

	objects, err := h.service.ListObjects(ctx, bucket, prefix)
	if errors.Is(err, fs.ErrBucketNotFound) {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err != nil {
		renderError(ctx, w, err)
		return
	}

	entries := buildListEntries(objects, prefix, delimiter)

	var (
		contents       []ObjectInfo
		commonPrefixes []CommonPrefix
		count          int
		truncated      bool
		nextCursor     string
	)

	maybeEncode := func(s string) string {
		if encodeURL {
			return url.QueryEscape(s)
		}

		return s
	}

	for _, e := range entries {
		if cursor != "" && e.key <= cursor {
			continue
		}

		if count >= maxKeys {
			truncated = true
			break
		}

		if e.isPrefix {
			commonPrefixes = append(commonPrefixes, CommonPrefix{Prefix: maybeEncode(e.key)})
		} else {
			contents = append(contents, ObjectInfo{
				Key:          maybeEncode(e.obj.Key),
				LastModified: e.obj.LastModified,
				ETag:         quoteETag(e.obj.ETag),
				Size:         e.obj.Size,
			})
		}

		nextCursor = e.key
		count++
	}

	resp := ListBucketResult{
		Name:           bucket,
		Prefix:         maybeEncode(prefix),
		Delimiter:      maybeEncode(delimiter),
		MaxKeys:        maxKeys,
		IsTruncated:    truncated,
		Contents:       contents,
		CommonPrefixes: commonPrefixes,
	}

	if encodeURL {
		resp.EncodingType = "url"
	}

	if isV2 {
		resp.KeyCount = count
		resp.ContinuationToken = q.Get("continuation-token")
		resp.StartAfter = maybeEncode(q.Get("start-after"))

		if truncated {
			resp.NextContinuationToken = encodeContinuationToken(nextCursor)
		}
	} else {
		resp.Marker = maybeEncode(q.Get("marker"))

		if truncated {
			resp.NextMarker = maybeEncode(nextCursor)
		}
	}

	writeXML(ctx, w, resp)
}

// buildListEntries folds objects into the ordered listing keyspace, rolling keys
// that contain the delimiter beyond the prefix into deduplicated common prefixes.
func buildListEntries(objects []fs.Object, prefix, delimiter string) []listEntry {
	entries := make([]listEntry, 0, len(objects))
	seenPrefix := make(map[string]struct{})

	for _, o := range objects {
		if delimiter != "" {
			rest := strings.TrimPrefix(o.Key, prefix)
			if idx := strings.Index(rest, delimiter); idx >= 0 {
				cp := prefix + rest[:idx+len(delimiter)]
				if _, ok := seenPrefix[cp]; !ok {
					seenPrefix[cp] = struct{}{}
					entries = append(entries, listEntry{key: cp, isPrefix: true})
				}

				continue
			}
		}

		entries = append(entries, listEntry{key: o.Key, obj: o})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].key < entries[j].key
	})

	return entries
}

// encodeContinuationToken returns an opaque, URL-safe pagination token for a key.
func encodeContinuationToken(key string) string {
	return base64.URLEncoding.EncodeToString([]byte(key))
}

// decodeContinuationToken reverses encodeContinuationToken, tolerating malformed
// input by treating it as a raw key.
func decodeContinuationToken(token string) string {
	if b, err := base64.URLEncoding.DecodeString(token); err == nil {
		return string(b)
	}

	return token
}

// writeXML writes an S3 XML response with the standard header.
func writeXML(ctx context.Context, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte(xml.Header)); err != nil {
		renderError(ctx, w, err)
		return
	}

	if err := xml.NewEncoder(w).Encode(v); err != nil {
		renderError(ctx, w, err)
		return
	}
}
