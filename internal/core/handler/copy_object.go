package handler

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// CopyObjectResult is the XML response for a CopyObject operation.
type CopyObjectResult struct {
	XMLName      xml.Name  `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyObjectResult"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
}

// CopyObject implements server-side copy, signaled by the x-amz-copy-source
// header on a PUT. It composes a read of the source and a write to the
// destination. Conditional-copy headers (x-amz-copy-source-if-*) are ignored.
func (h *handler) CopyObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	destBucket, destKey, _ := strings.Cut(path, "/")

	srcBucket, srcKey, ok := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if !ok {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, errors.New("invalid x-amz-copy-source"))
		return
	}

	src, err := h.service.GetObject(ctx, srcBucket, srcKey)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	defer func() { _ = src.Reader.Close() }()

	put := &fs.PutObjectRequest{
		Reader: src.Reader,
		Bucket: destBucket,
		Key:    destKey,
		Size:   src.Size,
	}
	if err := h.service.PutObject(ctx, put); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	// Read back the destination metadata for the response.
	etag, lastModified := src.ETag, time.Now().UTC()
	if dst, err := h.service.GetObject(ctx, destBucket, destKey); err == nil {
		etag, lastModified = dst.ETag, dst.LastModified
		_ = dst.Reader.Close()
	}

	writeXML(ctx, w, r, CopyObjectResult{
		LastModified: lastModified.UTC(),
		ETag:         quoteETag(etag),
	})
}

// parseCopySource parses an x-amz-copy-source value of the form "/bucket/key" or
// "bucket/key", tolerating a leading slash, URL-encoding, and a trailing
// ?versionId. The bucket and key are URL-decoded independently so encoded
// slashes inside the key are preserved.
func parseCopySource(s string) (bucket, key string, ok bool) {
	if s == "" {
		return "", "", false
	}

	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}

	s = strings.TrimPrefix(s, "/")

	b, k, found := strings.Cut(s, "/")
	if !found {
		return "", "", false
	}

	if decoded, err := url.QueryUnescape(b); err == nil {
		b = decoded
	}

	if decoded, err := url.QueryUnescape(k); err == nil {
		k = decoded
	}

	if b == "" || k == "" {
		return "", "", false
	}

	return b, k, true
}
