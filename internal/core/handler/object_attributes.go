package handler

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/checksum"
	"github.com/go-faster/fs/internal/s3err"
)

// storageClassStandard is the only storage class this server has. It is
// reported wherever S3 reports one so clients that switch on it see a value
// they know.
const storageClassStandard = "STANDARD"

// GetObjectAttributesResult is the XML body of GetObjectAttributes. Every
// element is omitted unless the caller asked for it through
// x-amz-object-attributes: S3 returns only the requested attributes, and a
// client that did not ask for ObjectParts must not receive it.
type GetObjectAttributesResult struct {
	XMLName      xml.Name           `xml:"http://s3.amazonaws.com/doc/2006-03-01/ GetObjectAttributesOutput"`
	ETag         string             `xml:"ETag,omitempty"`
	Checksum     *ChecksumResult    `xml:"Checksum,omitempty"`
	StorageClass string             `xml:"StorageClass,omitempty"`
	ObjectSize   *int64             `xml:"ObjectSize,omitempty"`
	ObjectParts  *ObjectPartsResult `xml:"ObjectParts,omitempty"`
}

// ChecksumResult is the object's client-visible digest as an attribute.
//
// Reported here without the x-amz-checksum-mode header a read needs, because
// asking for the Checksum attribute *is* the request for it — the header
// exists so an ordinary GET does not carry one, and this is not one.
type ChecksumResult struct {
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
	ChecksumType      string `xml:"ChecksumType,omitempty"`
}

// objectChecksumResult renders a digest under the element its algorithm owns,
// or nil when the object carries none.
func objectChecksumResult(algorithm, digest, kind string) *ChecksumResult {
	if digest == "" {
		return nil
	}

	out := &ChecksumResult{ChecksumType: kind}

	switch checksum.Algorithm(algorithm) {
	case checksum.CRC32:
		out.ChecksumCRC32 = digest
	case checksum.CRC32C:
		out.ChecksumCRC32C = digest
	case checksum.CRC64NVME:
		out.ChecksumCRC64NVME = digest
	case checksum.SHA1:
		out.ChecksumSHA1 = digest
	case checksum.SHA256:
		out.ChecksumSHA256 = digest
	default:
		return nil
	}

	return out
}

// ObjectPartsResult is the paginated part list of a completed multipart object.
//
// The count element is named PartsCount on the wire even though every SDK
// surfaces it as TotalPartsCount — the S3 model carries an explicit
// locationName for it, and a client parsing by wire name finds nothing under
// the documented one.
type ObjectPartsResult struct {
	TotalPartsCount      int              `xml:"PartsCount"`
	PartNumberMarker     int              `xml:"PartNumberMarker"`
	NextPartNumberMarker int              `xml:"NextPartNumberMarker"`
	MaxParts             int              `xml:"MaxParts"`
	IsTruncated          bool             `xml:"IsTruncated"`
	Parts                []ObjectPartsXML `xml:"Part"`
}

// ObjectPartsXML is one entry of that list.
type ObjectPartsXML struct {
	PartNumber int   `xml:"PartNumber"`
	Size       int64 `xml:"Size"`
}

// defaultMaxParts matches the S3 default and cap for a part listing.
const defaultMaxParts = 1000

// GetObjectAttributes serves GET ?attributes: the object's ETag, size, storage
// class and part layout, without opening the body.
//
// It needs a backend that can report the layout (fs.ObjectAttributer); one that
// cannot gets a typed NotImplemented rather than a plausible-looking answer
// with the parts silently missing.
func (h *handler) GetObjectAttributes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")

	attributer, ok := h.service.(fs.ObjectAttributer)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)
		return
	}

	attrs, err := attributer.ObjectAttributes(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	wanted := requestedAttributes(r.Header.Get("x-amz-object-attributes"))
	if len(wanted) == 0 {
		renderAPIError(ctx, w, r, s3err.InvalidArgument,
			errors.New("x-amz-object-attributes is required"))

		return
	}

	result := GetObjectAttributesResult{}

	if wanted["etag"] {
		result.ETag = strings.Trim(attrs.ETag, `"`)
	}

	if wanted["checksum"] {
		result.Checksum = objectChecksumResult(
			attrs.ChecksumAlgorithm, attrs.Checksum, attrs.ChecksumType)
	}

	if wanted["storageclass"] {
		result.StorageClass = storageClassStandard
	}

	if wanted["objectsize"] {
		size := attrs.Size
		result.ObjectSize = &size
	}

	// ObjectParts is reported only for an object that actually has parts: for a
	// single PUT, S3 omits the element entirely rather than reporting one part.
	if wanted["objectparts"] && len(attrs.Parts) > 0 {
		parts, err := renderObjectParts(r, attrs.Parts)
		if err != nil {
			renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
			return
		}

		result.ObjectParts = parts
	}

	w.Header().Set("Last-Modified", attrs.LastModified.UTC().Format(http.TimeFormat))
	writeXML(ctx, w, r, result)
}

// renderObjectParts applies the pagination to the layout.
//
// Unlike every other paginated S3 listing, GetObjectAttributes takes its
// bounds in headers (x-amz-max-parts, x-amz-part-number-marker) rather than
// query parameters.
func renderObjectParts(r *http.Request, layout []fs.ObjectPart) (*ObjectPartsResult, error) {
	maxParts := defaultMaxParts

	if raw := r.Header.Get("x-amz-max-parts"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, errors.Wrap(errMalformedCondition, "x-amz-max-parts")
		}

		if n < maxParts {
			maxParts = n
		}
	}

	var marker int

	if raw := r.Header.Get("x-amz-part-number-marker"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, errors.Wrap(errMalformedCondition, "x-amz-part-number-marker")
		}

		marker = n
	}

	out := &ObjectPartsResult{
		TotalPartsCount:  len(layout),
		PartNumberMarker: marker,
		MaxParts:         maxParts,
		Parts:            []ObjectPartsXML{},
	}

	for _, p := range layout {
		if p.PartNumber <= marker {
			continue
		}

		if len(out.Parts) >= maxParts {
			out.IsTruncated = true

			break
		}

		out.Parts = append(out.Parts, ObjectPartsXML{PartNumber: p.PartNumber, Size: p.Size})
		out.NextPartNumberMarker = p.PartNumber
	}

	return out, nil
}

// requestedAttributes parses the x-amz-object-attributes header into a set of
// lowercased names.
func requestedAttributes(header string) map[string]bool {
	wanted := make(map[string]bool)

	for name := range strings.SplitSeq(header, ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			wanted[name] = true
		}
	}

	return wanted
}
