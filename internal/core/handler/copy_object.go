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
// destination. Metadata follows x-amz-metadata-directive (COPY by default,
// REPLACE takes it from the request headers), tags follow
// x-amz-tagging-directive the same way. Conditional-copy headers
// (x-amz-copy-source-if-*) are evaluated against the source object.
func (h *handler) CopyObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	destBucket, destKey, _ := strings.Cut(path, "/")

	srcBucket, srcKey, ok := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if !ok {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, errors.New("invalid x-amz-copy-source"))
		return
	}

	metadataDirective, ok := parseDirective(r.Header.Get("X-Amz-Metadata-Directive"))
	if !ok {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, errors.New("invalid x-amz-metadata-directive"))
		return
	}

	taggingDirective, ok := parseDirective(r.Header.Get("X-Amz-Tagging-Directive"))
	if !ok {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, errors.New("invalid x-amz-tagging-directive"))
		return
	}

	// Copying an object onto itself is only allowed when it changes something
	// (metadata REPLACE), matching S3.
	if srcBucket == destBucket && srcKey == destKey && metadataDirective != directiveReplace {
		renderAPIError(ctx, w, r, s3err.InvalidRequest,
			errors.New("copy to itself without metadata directive REPLACE"))

		return
	}

	src, err := h.service.GetObject(ctx, srcBucket, srcKey)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	defer func() { _ = src.Reader.Close() }()

	if !copySourceConditionsHold(r.Header, src) {
		renderAPIError(ctx, w, r, s3err.PreconditionFailed,
			errors.New("x-amz-copy-source-if-* condition failed"))

		return
	}

	metadata := src.Metadata
	if metadataDirective == directiveReplace {
		metadata = extractObjectMetadata(r.Header)
	}

	var tags []fs.Tag

	if taggingDirective == directiveReplace {
		if tags, err = parseTaggingHeader(r.Header.Get("X-Amz-Tagging")); err != nil {
			renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
			return
		}
	} else if tags, err = h.service.GetObjectTagging(ctx, srcBucket, srcKey); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	put := &fs.PutObjectRequest{
		Reader:   src.Reader,
		Bucket:   destBucket,
		Key:      destKey,
		Size:     src.Size,
		Metadata: metadata,
		Tags:     tags,
		ACL:      fs.ParseACL(r.Header.Get("X-Amz-Acl")),
		Owner:    callerOwner(ctx),
	}

	resp, err := h.service.PutObject(ctx, put)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	// Read back the destination for the response timestamp.
	lastModified := time.Now().UTC()
	if dst, err := h.service.GetObject(ctx, destBucket, destKey); err == nil {
		lastModified = dst.LastModified
		_ = dst.Reader.Close()
	}

	writeXML(ctx, w, r, CopyObjectResult{
		LastModified: lastModified.UTC(),
		ETag:         quoteETag(resp.ETag),
	})
}

// copySourceConditionsHold evaluates the x-amz-copy-source-if-* headers
// against the source object, returning false when the copy must be rejected
// with 412.
//
// The pairing rules are S3's: an ETag condition takes precedence over the
// timestamp condition it is paired with, so if-match wins over
// if-unmodified-since and if-none-match wins over if-modified-since. A client
// sending both is asking "copy if it is still the object I saw", and the ETag
// answers that more precisely than a one-second timestamp can.
func copySourceConditionsHold(header http.Header, src *fs.GetObjectResponse) bool {
	var (
		ifMatch     = strings.TrimSpace(header.Get("X-Amz-Copy-Source-If-Match"))
		ifNoneMatch = strings.TrimSpace(header.Get("X-Amz-Copy-Source-If-None-Match"))
		unmodified  = strings.TrimSpace(header.Get("X-Amz-Copy-Source-If-Unmodified-Since"))
		modified    = strings.TrimSpace(header.Get("X-Amz-Copy-Source-If-Modified-Since"))
	)

	state := fs.ObjectState{Exists: true, ETag: src.ETag, Size: src.Size, LastModified: src.LastModified}

	switch {
	case ifMatch != "":
		if err := (fs.Conditions{IfMatch: ifMatch}).CheckWrite(state); err != nil {
			return false
		}
	case unmodified != "":
		if t, err := http.ParseTime(unmodified); err == nil &&
			src.LastModified.Truncate(time.Second).After(t) {
			return false
		}
	}

	switch {
	case ifNoneMatch != "":
		if err := (fs.Conditions{IfNoneMatch: ifNoneMatch}).CheckWrite(state); err != nil {
			return false
		}
	case modified != "":
		if t, err := http.ParseTime(modified); err == nil &&
			!src.LastModified.Truncate(time.Second).After(t) {
			return false
		}
	}

	return true
}

// Copy directives for metadata and tagging.
const (
	directiveCopy    = "COPY"
	directiveReplace = "REPLACE"
)

// parseDirective normalizes an x-amz-*-directive header value; empty means COPY.
func parseDirective(value string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "", directiveCopy:
		return directiveCopy, true
	case directiveReplace:
		return directiveReplace, true
	default:
		return "", false
	}
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
