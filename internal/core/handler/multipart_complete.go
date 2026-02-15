package handler

import (
	"encoding/xml"
	"net/http"
	"sort"
	"strings"

	"github.com/go-faster/fs"
)

// CompleteMultipartUploadResult represents the response for completing multipart upload.
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// CompleteMultipartUploadXML represents the XML request body for completing multipart upload.
type CompleteMultipartUploadXML struct {
	XMLName xml.Name           `xml:"CompleteMultipartUpload"`
	Parts   []CompletedPartXML `xml:"Part"`
}

// CompletedPartXML represents a part in the completion request.
type CompletedPartXML struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

func (h *handler) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	ctx := r.Context()

	// Parse the XML body.
	var xmlReq CompleteMultipartUploadXML
	if err := xml.NewDecoder(r.Body).Decode(&xmlReq); err != nil {
		renderError(ctx, w, err)
		return
	}

	// Convert to internal format.
	parts := make([]fs.CompletedPart, len(xmlReq.Parts))
	for i, p := range xmlReq.Parts {
		// Remove quotes from ETag if present.
		etag := strings.Trim(p.ETag, `"`)
		parts[i] = fs.CompletedPart{
			PartNumber: p.PartNumber,
			ETag:       etag,
		}
	}

	// Sort parts by part number.
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})

	req := &fs.CompleteMultipartUploadRequest{
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
		Parts:    parts,
	}

	resp, err := h.service.CompleteMultipartUpload(ctx, req)
	if err != nil {
		renderError(ctx, w, err)
		return
	}

	result := CompleteMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Location: resp.Location,
		Bucket:   resp.Bucket,
		Key:      resp.Key,
		ETag:     `"` + resp.ETag + `"`,
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}
