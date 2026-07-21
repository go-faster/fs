package handler

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// CopyPartResult is the XML response for an UploadPartCopy operation.
type CopyPartResult struct {
	XMLName      xml.Name  `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyPartResult"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
}

// UploadPartCopy handles PUT with ?partNumber&uploadId and x-amz-copy-source:
// it uploads a part by copying (a range of) an existing object. The part's
// ETag is recomputed from the copied bytes by the storage layer.
func (h *handler) UploadPartCopy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()

	partNumber, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
		return
	}

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

	reader, size := io.Reader(src.Reader), src.Size

	if rangeHeader := r.Header.Get("X-Amz-Copy-Source-Range"); rangeHeader != "" {
		first, last, err := parseCopySourceRange(rangeHeader)
		if err != nil {
			renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
			return
		}

		if first >= src.Size || last >= src.Size {
			renderAPIError(ctx, w, r, s3err.InvalidRange,
				errors.Errorf("range %d-%d exceeds source size %d", first, last, src.Size))

			return
		}

		if err := skipBytes(src.Reader, first); err != nil {
			renderError(ctx, w, r, err)
			return
		}

		size = last - first + 1
		reader = io.LimitReader(src.Reader, size)
	}

	part, err := h.service.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket:     bucket,
		Key:        key,
		UploadID:   q.Get("uploadId"),
		PartNumber: partNumber,
		Reader:     reader,
		Size:       size,
	})
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	writeXML(ctx, w, r, CopyPartResult{
		LastModified: part.LastModified.UTC(),
		ETag:         quoteETag(part.ETag),
	})
}

// parseCopySourceRange parses an x-amz-copy-source-range value of the strict
// form "bytes=first-last" with both bounds present and first <= last.
func parseCopySourceRange(s string) (first, last int64, _ error) {
	spec, ok := strings.CutPrefix(s, "bytes=")
	if !ok {
		return 0, 0, errors.Errorf("invalid copy source range %q", s)
	}

	firstStr, lastStr, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, errors.Errorf("invalid copy source range %q", s)
	}

	first, err := strconv.ParseInt(firstStr, 10, 64)
	if err != nil {
		return 0, 0, errors.Errorf("invalid copy source range %q", s)
	}

	last, err = strconv.ParseInt(lastStr, 10, 64)
	if err != nil || first < 0 || last < first {
		return 0, 0, errors.Errorf("invalid copy source range %q", s)
	}

	return first, last, nil
}

// skipBytes advances r by n bytes, seeking when possible.
func skipBytes(r io.Reader, n int64) error {
	if n == 0 {
		return nil
	}

	if seeker, ok := r.(io.Seeker); ok {
		if _, err := seeker.Seek(n, io.SeekStart); err != nil {
			return errors.Wrap(err, "seek to range start")
		}

		return nil
	}

	if _, err := io.CopyN(io.Discard, r, n); err != nil {
		return errors.Wrap(err, "skip to range start")
	}

	return nil
}
