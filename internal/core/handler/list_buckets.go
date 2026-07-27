package handler

import (
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/s3err"
)

// ObjectInfo is the XML representation of an object in a bucket listing.
type ObjectInfo struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag,omitempty"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass,omitempty"`
	// Owner is present on V1 listings and on V2 only when fetch-owner=true,
	// which is what S3 does.
	Owner *OwnerXML `xml:"Owner,omitempty"`
}

// CommonPrefix is a grouped key prefix produced by delimiter-based listing.
type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// ListBucketResult is the XML response for ListObjects (v1) and ListObjectsV2.
type ListBucketResult struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name    string   `xml:"Name"`
	Prefix  string   `xml:"Prefix"`

	// V2 pagination. ContinuationToken is a pointer so an empty token can be
	// echoed as an empty element: a client that sent one expects to see it
	// back, and omitting the element reads as "never sent".
	ContinuationToken     *string `xml:"ContinuationToken"`
	NextContinuationToken string  `xml:"NextContinuationToken,omitempty"`
	StartAfter            string  `xml:"StartAfter,omitempty"`
	// KeyCount is set on every V2 response (including zero) and never on V1.
	KeyCount *int `xml:"KeyCount,omitempty"`

	// V1 pagination. Marker is always present on a V1 response, empty or not,
	// which is why it is a pointer: V2 responses must not carry it at all.
	Marker     *string `xml:"Marker"`
	NextMarker string  `xml:"NextMarker,omitempty"`

	MaxKeys      int    `xml:"MaxKeys"`
	Delimiter    string `xml:"Delimiter,omitempty"`
	EncodingType string `xml:"EncodingType,omitempty"`
	IsTruncated  bool   `xml:"IsTruncated"`

	Contents       []ObjectInfo   `xml:"Contents"`
	CommonPrefixes []CommonPrefix `xml:"CommonPrefixes"`
}

// BucketsWrapper wraps the list of buckets.
type BucketsWrapper struct {
	Buckets []BucketInfo `xml:"Bucket"`
}

// Bucket represents an S3 bucket.

// ListAllMyBucketsResult is the XML response for listing buckets.
type ListAllMyBucketsResult struct {
	XMLName xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
	Buckets BucketsWrapper `xml:"Buckets"`
	// ContinuationToken is present only when more buckets follow, so its
	// absence is how a client knows the listing is complete.
	ContinuationToken string `xml:"ContinuationToken,omitempty"`
	Prefix            string `xml:"Prefix,omitempty"`
}

// maxBucketsPerPage is the cap S3 applies to a bucket listing page.
const maxBucketsPerPage = 10000

func (h *handler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	buckets, err := h.service.ListBuckets(ctx)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	q := r.URL.Query()

	// Buckets come back in whatever order the backend keeps them; paging needs
	// a stable one, and name order is the one S3 reports.
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })

	prefix := q.Get("prefix")

	maxBuckets := maxBucketsPerPage

	if raw := q.Get("max-buckets"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			renderAPIError(ctx, w, r, s3err.InvalidArgument,
				errors.Errorf("invalid max-buckets %q", raw))

			return
		}

		if n < maxBuckets {
			maxBuckets = n
		}
	}

	// The token is the last name of the previous page: an exclusive lower
	// bound, so a bucket created or deleted between pages cannot shift the
	// window and hide another one.
	after := decodeContinuationToken(q.Get("continuation-token"))

	response := ListAllMyBucketsResult{Prefix: prefix}

	for _, bucket := range buckets {
		if prefix != "" && !strings.HasPrefix(bucket.Name, prefix) {
			continue
		}

		if after != "" && bucket.Name <= after {
			continue
		}

		if len(response.Buckets.Buckets) >= maxBuckets {
			response.ContinuationToken = encodeContinuationToken(
				response.Buckets.Buckets[len(response.Buckets.Buckets)-1].Name)

			break
		}

		response.Buckets.Buckets = append(response.Buckets.Buckets, BucketInfo(bucket))
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte(xml.Header)); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	if err := xml.NewEncoder(w).Encode(response); err != nil {
		renderError(ctx, w, r, err)
		return
	}
}
