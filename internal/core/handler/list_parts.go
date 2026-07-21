package handler

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/s3err"
)

// ListPartsResult is the XML response for a ListParts operation.
type ListPartsResult struct {
	XMLName              xml.Name  `xml:"ListPartsResult"`
	Xmlns                string    `xml:"xmlns,attr"`
	Bucket               string    `xml:"Bucket"`
	Key                  string    `xml:"Key"`
	UploadID             string    `xml:"UploadId"`
	StorageClass         string    `xml:"StorageClass"`
	PartNumberMarker     int       `xml:"PartNumberMarker"`
	NextPartNumberMarker int       `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int       `xml:"MaxParts"`
	IsTruncated          bool      `xml:"IsTruncated"`
	Parts                []PartXML `xml:"Part"`
}

// PartXML is a single part entry in a ListPartsResult.
type PartXML struct {
	PartNumber   int       `xml:"PartNumber"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
}

// ListParts handles GET on an object with ?uploadId, returning the parts
// uploaded so far, paginated by part-number-marker/max-parts.
func (h *handler) ListParts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()
	uploadID := q.Get("uploadId")

	maxParts := defaultMaxKeys

	if v := q.Get("max-parts"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			renderAPIError(ctx, w, r, s3err.InvalidArgument, errors.New("invalid max-parts"))
			return
		}

		if n < maxParts {
			maxParts = n
		}
	}

	marker := 0

	if v := q.Get("part-number-marker"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			renderAPIError(ctx, w, r, s3err.InvalidArgument, errors.New("invalid part-number-marker"))
			return
		}

		marker = n
	}

	parts, err := h.service.ListParts(ctx, bucket, key, uploadID)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	resp := ListPartsResult{
		Xmlns:            "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:           bucket,
		Key:              key,
		UploadID:         uploadID,
		StorageClass:     "STANDARD",
		PartNumberMarker: marker,
		MaxParts:         maxParts,
	}

	for _, p := range parts {
		// part-number-marker is exclusive.
		if p.PartNumber <= marker {
			continue
		}

		if len(resp.Parts) >= maxParts {
			resp.IsTruncated = true
			break
		}

		resp.Parts = append(resp.Parts, PartXML{
			PartNumber:   p.PartNumber,
			LastModified: p.LastModified.UTC(),
			ETag:         quoteETag(p.ETag),
			Size:         p.Size,
		})
	}

	if resp.IsTruncated && len(resp.Parts) > 0 {
		resp.NextPartNumberMarker = resp.Parts[len(resp.Parts)-1].PartNumber
	}

	writeXML(ctx, w, r, resp)
}
