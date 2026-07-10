package handler

import (
	"encoding/xml"
	"net/http"
	"time"
)

// ObjectInfo is the XML representation of an object in a bucket listing.
type ObjectInfo struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag,omitempty"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass,omitempty"`
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

	// V2 pagination.
	ContinuationToken     string `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string `xml:"NextContinuationToken,omitempty"`
	StartAfter            string `xml:"StartAfter,omitempty"`
	KeyCount              int    `xml:"KeyCount,omitempty"`

	// V1 pagination.
	Marker     string `xml:"Marker,omitempty"`
	NextMarker string `xml:"NextMarker,omitempty"`

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
}

func (h *handler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	buckets, err := h.service.ListBuckets(ctx)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	bucketInfos := make([]BucketInfo, len(buckets))
	for i, bucket := range buckets {
		bucketInfos[i] = BucketInfo(bucket)
	}

	response := ListAllMyBucketsResult{
		Buckets: BucketsWrapper{
			Buckets: bucketInfos,
		},
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
