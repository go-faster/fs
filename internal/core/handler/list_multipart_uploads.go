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

// ListMultipartUploadsResult is the XML response for a ListMultipartUploads
// operation.
type ListMultipartUploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	Xmlns              string         `xml:"xmlns,attr"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIDMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker,omitempty"`
	Delimiter          string         `xml:"Delimiter,omitempty"`
	Prefix             string         `xml:"Prefix"`
	EncodingType       string         `xml:"EncodingType,omitempty"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	Uploads            []UploadXML    `xml:"Upload"`
	CommonPrefixes     []CommonPrefix `xml:"CommonPrefixes"`
}

// UploadXML is a single in-progress upload entry in a
// ListMultipartUploadsResult.
type UploadXML struct {
	Key          string    `xml:"Key"`
	UploadID     string    `xml:"UploadId"`
	StorageClass string    `xml:"StorageClass"`
	Initiated    time.Time `xml:"Initiated"`
}

// ListMultipartUploads handles GET on a bucket with ?uploads, returning
// in-progress multipart uploads ordered by key then upload ID, with
// prefix/delimiter grouping and key-marker/upload-id-marker pagination.
func (h *handler) ListMultipartUploads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, _, _ := strings.Cut(path, "/")

	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	keyMarker := q.Get("key-marker")
	uploadIDMarker := q.Get("upload-id-marker")

	encodeURL, err := parseEncodingType(q)
	if err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
		return
	}

	maxUploads := defaultMaxKeys

	if v := q.Get("max-uploads"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			renderAPIError(ctx, w, r, s3err.InvalidArgument, errors.New("invalid max-uploads"))
			return
		}

		if n < maxUploads {
			maxUploads = n
		}
	}

	uploads, err := h.service.ListMultipartUploads(ctx, bucket)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	maybeEncode := func(s string) string {
		if encodeURL {
			return s3EncodeKey(s)
		}

		return s
	}

	resp := ListMultipartUploadsResult{
		Xmlns:          "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:         bucket,
		KeyMarker:      maybeEncode(keyMarker),
		UploadIDMarker: uploadIDMarker,
		Delimiter:      maybeEncode(delimiter),
		Prefix:         maybeEncode(prefix),
		MaxUploads:     maxUploads,
	}

	if encodeURL {
		resp.EncodingType = encodingTypeURL
	}

	var (
		count                 int
		lastKey, lastUploadID string
		seenPrefix            = make(map[string]struct{})
	)

	for _, u := range uploads {
		if prefix != "" && !strings.HasPrefix(u.Key, prefix) {
			continue
		}

		// Markers are exclusive: skip up to and including (key-marker,
		// upload-id-marker). A key-marker alone skips all uploads for keys up
		// to and including it.
		if keyMarker != "" {
			if u.Key < keyMarker {
				continue
			}

			if u.Key == keyMarker && (uploadIDMarker == "" || u.UploadID <= uploadIDMarker) {
				continue
			}
		}

		// Delimiter grouping: keys with the delimiter beyond the prefix are
		// rolled up into common prefixes, which count toward max-uploads.
		if delimiter != "" {
			rest := strings.TrimPrefix(u.Key, prefix)
			if idx := strings.Index(rest, delimiter); idx >= 0 {
				cp := prefix + rest[:idx+len(delimiter)]
				if _, ok := seenPrefix[cp]; ok {
					continue
				}

				if count >= maxUploads {
					resp.IsTruncated = true
					break
				}

				seenPrefix[cp] = struct{}{}
				resp.CommonPrefixes = append(resp.CommonPrefixes, CommonPrefix{Prefix: maybeEncode(cp)})
				lastKey, lastUploadID = u.Key, u.UploadID
				count++

				continue
			}
		}

		if count >= maxUploads {
			resp.IsTruncated = true
			break
		}

		resp.Uploads = append(resp.Uploads, UploadXML{
			Key:          maybeEncode(u.Key),
			UploadID:     u.UploadID,
			StorageClass: "STANDARD",
			Initiated:    u.Initiated.UTC(),
		})
		lastKey, lastUploadID = u.Key, u.UploadID
		count++
	}

	if resp.IsTruncated {
		resp.NextKeyMarker = maybeEncode(lastKey)
		resp.NextUploadIDMarker = lastUploadID
	}

	writeXML(ctx, w, r, resp)
}
