package handler

import (
	"encoding/xml"
	"net/http"
	"time"
)

// ObjectInfo is the XML representation of an object.
type ObjectInfo struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag,omitempty"`
}

// ListBucketResult is the XML response for listing objects in a bucket.
type ListBucketResult struct {
	XMLName  xml.Name     `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name     string       `xml:"Name"`
	Contents []ObjectInfo `xml:"Contents"`
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
		renderError(ctx, w, err)
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
		renderError(ctx, w, err)
		return
	}
	if err := xml.NewEncoder(w).Encode(response); err != nil {
		renderError(ctx, w, err)
		return
	}
}
