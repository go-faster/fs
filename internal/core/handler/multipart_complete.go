package handler

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/checksum"
	"github.com/go-faster/fs/internal/s3err"
)

// CompleteMultipartUploadResult represents the response for completing multipart upload.
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
	// The completed object's client-visible digest, in the body rather than a
	// header — which is where the S3 API puts it for this one operation, and
	// what botocore reads. Only one of the algorithm elements is ever set.
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
	ChecksumType      string `xml:"ChecksumType,omitempty"`
}

// setChecksum puts a digest under the element its algorithm owns.
func (r *CompleteMultipartUploadResult) setChecksum(algorithm, digest string) {
	switch checksum.Algorithm(algorithm) {
	case checksum.CRC32:
		r.ChecksumCRC32 = digest
	case checksum.CRC32C:
		r.ChecksumCRC32C = digest
	case checksum.CRC64NVME:
		r.ChecksumCRC64NVME = digest
	case checksum.SHA1:
		r.ChecksumSHA1 = digest
	case checksum.SHA256:
		r.ChecksumSHA256 = digest
	}
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
	// The digest the client says this part had. One element per algorithm, and
	// the client sends whichever its upload used; digest() picks out the one
	// that is set rather than requiring the reader to know which to look at.
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
}

// digest is whichever per-algorithm element the client filled in.
func (p CompletedPartXML) digest() string {
	for _, v := range []string{
		p.ChecksumCRC32, p.ChecksumCRC32C, p.ChecksumCRC64NVME, p.ChecksumSHA1, p.ChecksumSHA256,
	} {
		if v != "" {
			return v
		}
	}

	return ""
}

func (h *handler) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	ctx := r.Context()

	// Parse the XML body.
	var xmlReq CompleteMultipartUploadXML
	if err := xml.NewDecoder(r.Body).Decode(&xmlReq); err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	if len(xmlReq.Parts) == 0 {
		renderAPIError(ctx, w, r, s3err.MalformedXML, errors.New("empty part list"))
		return
	}

	// Convert to internal format. S3 does not sort for the client: the list
	// must already be in strictly ascending part-number order.
	parts := make([]fs.CompletedPart, len(xmlReq.Parts))
	for i, p := range xmlReq.Parts {
		// Remove quotes from ETag if present.
		etag := strings.Trim(p.ETag, `"`)
		parts[i] = fs.CompletedPart{
			PartNumber: p.PartNumber,
			ETag:       etag,
			Checksum:   p.digest(),
		}

		if i > 0 && parts[i].PartNumber <= parts[i-1].PartNumber {
			renderAPIError(ctx, w, r, s3err.InvalidPartOrder,
				errors.Errorf("part %d after part %d", parts[i].PartNumber, parts[i-1].PartNumber))

			return
		}
	}

	cond, err := requestConditions(r)
	if err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
		return
	}

	req := &fs.CompleteMultipartUploadRequest{
		Bucket:     bucket,
		Key:        key,
		UploadID:   uploadID,
		Parts:      parts,
		Conditions: cond,
	}

	// The completion may name the digest it expects the object to have. It is a
	// claim like any other: the server composes its own from the parts and
	// refuses a completion that disagrees.
	_, req.Checksum = requestChecksum(r)
	req.ChecksumType = strings.TrimSpace(r.Header.Get(checksumTypeHeader))

	resp, err := h.service.CompleteMultipartUpload(ctx, req)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	result := CompleteMultipartUploadResult{
		Xmlns:        "http://s3.amazonaws.com/doc/2006-03-01/",
		Location:     resp.Location,
		Bucket:       resp.Bucket,
		Key:          resp.Key,
		ETag:         `"` + resp.ETag + `"`,
		ChecksumType: resp.ChecksumType,
	}

	result.setChecksum(resp.ChecksumAlgorithm, resp.Checksum)

	w.Header().Set("Content-Type", "application/xml")
	writeSSE(w, resp.ServerSideEncryption)
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}
