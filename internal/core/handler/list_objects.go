package handler

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// defaultMaxKeys is the S3 default and maximum number of keys returned per page.
const defaultMaxKeys = 1000

// encodingTypeURL is the only supported value of the encoding-type parameter;
// when requested, echoed key fields are URL-encoded.
const encodingTypeURL = "url"

// listEntry is a single item in the ordered listing keyspace: either an object or
// a delimiter-derived common prefix.
type listEntry struct {
	key      string
	obj      fs.Object
	isPrefix bool
}

// listPage is the delimiter-and-pagination walk shared by ListObjects V1/V2.
type listPage struct {
	contents       []ObjectInfo
	commonPrefixes []CommonPrefix
	count          int
	truncated      bool
	nextCursor     string
}

// listQuery holds the parameters common to both listing versions.
type listQuery struct {
	bucket    string
	prefix    string
	delimiter string
	encodeURL bool
	maxKeys   int
}

// maybeEncode URL-encodes s when encoding-type=url was requested.
func (p *listQuery) maybeEncode(s string) string {
	if p.encodeURL {
		return s3EncodeKey(s)
	}

	return s
}

// parseListQuery parses the shared listing parameters, rejecting invalid
// max-keys and unknown encoding-type values.
func parseListQuery(r *http.Request) (*listQuery, error) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")
	q := r.URL.Query()

	encodeURL, err := parseEncodingType(q)
	if err != nil {
		return nil, err
	}

	maxKeys := defaultMaxKeys

	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, errors.Errorf("invalid max-keys %q", v)
		}

		// Values above 1000 are clamped, not rejected.
		if n < maxKeys {
			maxKeys = n
		}
	}

	return &listQuery{
		bucket:    bucket,
		prefix:    q.Get("prefix"),
		delimiter: q.Get("delimiter"),
		encodeURL: encodeURL,
		maxKeys:   maxKeys,
	}, nil
}

// parseEncodingType validates the encoding-type parameter: absent (false) or
// "url" (true); anything else is an error.
func parseEncodingType(q map[string][]string) (bool, error) {
	values, ok := q["encoding-type"]
	if !ok || len(values) == 0 {
		return false, nil
	}

	if values[0] != encodingTypeURL {
		return false, errors.Errorf("invalid encoding-type %q", values[0])
	}

	return true, nil
}

// walkList pages through the delimiter-folded keyspace after the exclusive
// cursor, encoding output fields as requested. Common prefixes count toward
// maxKeys, exactly like on S3.
func (h *handler) walkList(ctx context.Context, p *listQuery, cursor string) (*listPage, error) {
	objects, err := h.service.ListObjects(ctx, p.bucket, p.prefix)
	if err != nil {
		return nil, err
	}

	entries := buildListEntries(objects, p.prefix, p.delimiter)
	page := &listPage{}

	// S3 answers max-keys=0 with an empty, non-truncated result.
	if p.maxKeys == 0 {
		return page, nil
	}

	for _, e := range entries {
		if cursor != "" && e.key <= cursor {
			continue
		}

		if page.count >= p.maxKeys {
			page.truncated = true
			break
		}

		if e.isPrefix {
			page.commonPrefixes = append(page.commonPrefixes, CommonPrefix{Prefix: p.maybeEncode(e.key)})
		} else {
			page.contents = append(page.contents, ObjectInfo{
				Key:          p.maybeEncode(e.obj.Key),
				LastModified: e.obj.LastModified,
				ETag:         quoteETag(e.obj.ETag),
				Size:         e.obj.Size,
			})
		}

		page.nextCursor = e.key
		page.count++
	}

	return page, nil
}

// baseListResult fills the response fields shared by V1 and V2.
func baseListResult(p *listQuery, page *listPage) ListBucketResult {
	resp := ListBucketResult{
		Name:           p.bucket,
		Prefix:         p.maybeEncode(p.prefix),
		Delimiter:      p.maybeEncode(p.delimiter),
		MaxKeys:        p.maxKeys,
		IsTruncated:    page.truncated,
		Contents:       page.contents,
		CommonPrefixes: page.commonPrefixes,
	}

	if p.encodeURL {
		resp.EncodingType = encodingTypeURL
	}

	return resp
}

// ListObjectsV1 handles GET on a bucket without list-type=2.
func (h *handler) ListObjectsV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	p, err := parseListQuery(r)
	if err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
		return
	}

	marker := r.URL.Query().Get("marker")

	page, err := h.walkList(ctx, p, marker)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	resp := baseListResult(p, page)
	resp.Marker = p.maybeEncode(marker)

	// V1 returns NextMarker only for delimiter listings; otherwise clients
	// continue from the last Contents key.
	if page.truncated && p.delimiter != "" {
		resp.NextMarker = p.maybeEncode(page.nextCursor)
	}

	writeXML(ctx, w, r, resp)
}

// ListObjectsV2 handles GET on a bucket with list-type=2.
func (h *handler) ListObjectsV2(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	p, err := parseListQuery(r)
	if err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
		return
	}

	q := r.URL.Query()

	// The continuation token wins over start-after; both are exclusive bounds.
	cursor := q.Get("start-after")
	if token := q.Get("continuation-token"); token != "" {
		cursor = decodeContinuationToken(token)
	}

	page, err := h.walkList(ctx, p, cursor)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	resp := baseListResult(p, page)
	resp.KeyCount = &page.count
	resp.ContinuationToken = q.Get("continuation-token")
	resp.StartAfter = p.maybeEncode(q.Get("start-after"))

	if page.truncated {
		resp.NextContinuationToken = encodeContinuationToken(page.nextCursor)
	}

	writeXML(ctx, w, r, resp)
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

// s3EncodeKey URL-encodes an object key the way S3 does for encoding-type=url:
// RFC 3986 percent-encoding of every byte outside the unreserved set, with "/"
// left intact.
func s3EncodeKey(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			const hexDigits = "0123456789ABCDEF"

			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
	}

	return b.String()
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
func writeXML(ctx context.Context, w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte(xml.Header)); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	if err := xml.NewEncoder(w).Encode(v); err != nil {
		renderError(ctx, w, r, err)
		return
	}
}
