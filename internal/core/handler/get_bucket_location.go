package handler

import (
	"encoding/xml"
	"net/http"
)

// LocationConstraintResult is the XML response for GetBucketLocation. An empty
// constraint denotes the default region (us-east-1), which is what this
// single-region server always reports.
type LocationConstraintResult struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
	Value   string   `xml:",chardata"`
}

// GetBucketLocation implements GET /{bucket}?location, always reporting the
// default region. S3 SDKs (e.g. minio-go) call this to cache a bucket's region
// before other operations, so it stays cheap and unconditional rather than
// verifying the bucket exists.
func (h *handler) GetBucketLocation(w http.ResponseWriter, r *http.Request) {
	writeXML(r.Context(), w, r, LocationConstraintResult{})
}
