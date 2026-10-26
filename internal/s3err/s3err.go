// Package s3err renders S3-compatible error responses.
//
// AWS SDKs parse the XML <Error> document body and raise typed exceptions from
// its <Code> element, so wire-correct error responses (not a bespoke JSON
// shape) are what makes real clients work against the server.
package s3err

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// codeInvalidRequest is shared by the generic invalid-request error and the
// CORS preflight errors, which S3 reports under the same code.
const codeInvalidRequest = "InvalidRequest"

// APIError describes an S3 error: a stable wire code, its HTTP status, and a
// default human-readable message.
type APIError struct {
	Code       string
	HTTPStatus int
	Message    string
}

// Detail holds the elements S3 adds to an error body after Code and Message.
// Which ones appear depends on the code, as AWS S3 renders them: BucketName
// for NoSuchBucket, Key for NoSuchKey, Key and VersionId for NoSuchVersion, UploadId for NoSuchUpload, Condition
// for PreconditionFailed, RangeRequested and ActualObjectSize for
// InvalidRange, ArgumentName and ArgumentValue for InvalidArgument, Method and
// ResourceType for MethodNotAllowed. Empty elements are left out.
type Detail struct {
	BucketName       string `xml:"BucketName,omitempty"`
	Key              string `xml:"Key,omitempty"`
	VersionID        string `xml:"VersionId,omitempty"`
	UploadID         string `xml:"UploadId,omitempty"`
	Condition        string `xml:"Condition,omitempty"`
	RangeRequested   string `xml:"RangeRequested,omitempty"`
	ActualObjectSize *int64 `xml:"ActualObjectSize,omitempty"`
	ArgumentName     string `xml:"ArgumentName,omitempty"`
	ArgumentValue    string `xml:"ArgumentValue,omitempty"`
	Method           string `xml:"Method,omitempty"`
	ResourceType     string `xml:"ResourceType,omitempty"`
}

// merge fills d's empty elements from o.
func (d Detail) merge(o Detail) Detail {
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}

	fill(&d.BucketName, o.BucketName)
	fill(&d.Key, o.Key)
	fill(&d.VersionID, o.VersionID)
	fill(&d.UploadID, o.UploadID)
	fill(&d.Condition, o.Condition)
	fill(&d.RangeRequested, o.RangeRequested)
	fill(&d.ArgumentName, o.ArgumentName)
	fill(&d.ArgumentValue, o.ArgumentValue)
	fill(&d.Method, o.Method)
	fill(&d.ResourceType, o.ResourceType)

	if d.ActualObjectSize == nil {
		d.ActualObjectSize = o.ActualObjectSize
	}

	return d
}

// Size is a helper for Detail.ActualObjectSize.
func Size(n int64) *int64 { return &n }

// detailed is an error that carries the details of its S3 error body, for an
// error raised where they are known and rendered elsewhere.
type detailed struct {
	err    error
	detail Detail
}

func (d *detailed) Error() string { return d.err.Error() }
func (d *detailed) Unwrap() error { return d.err }

// WithDetail attaches d to err; Write renders it into the body of whatever
// error err maps to.
func WithDetail(err error, d Detail) error {
	if err == nil {
		return nil
	}

	return &detailed{err: err, detail: d}
}

// DetailOf returns the Detail attached to err with WithDetail, and the one
// a delete marker implies.
func DetailOf(err error) Detail {
	var d Detail

	var de *detailed
	if errors.As(err, &de) {
		d = de.detail
	}

	if errors.Is(err, fs.ErrMethodNotAllowedOnDeleteMarker) {
		d = d.merge(Detail{ResourceType: "DeleteMarker"})
	}

	return d
}

// The error table. These are the codes real SDK/client paths depend on; each
// maps to exactly one HTTP status and a default message.
var (
	NoSuchBucket                 = APIError{"NoSuchBucket", http.StatusNotFound, "The specified bucket does not exist"}
	NoSuchKey                    = APIError{"NoSuchKey", http.StatusNotFound, "The specified key does not exist."}
	NoSuchVersion                = APIError{"NoSuchVersion", http.StatusNotFound, "The specified version does not exist."}
	NoSuchUpload                 = APIError{"NoSuchUpload", http.StatusNotFound, "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed."}
	NoSuchBucketPolicy           = APIError{"NoSuchBucketPolicy", http.StatusNotFound, "The bucket policy does not exist."}
	NoSuchCORSConfiguration      = APIError{"NoSuchCORSConfiguration", http.StatusNotFound, "The CORS configuration does not exist"}
	NoSuchLifecycleConfiguration = APIError{
		"NoSuchLifecycleConfiguration", http.StatusNotFound,
		"The lifecycle configuration does not exist",
	}
	NoSuchPublicAccessBlock = APIError{
		"NoSuchPublicAccessBlockConfiguration", http.StatusNotFound,
		"The public access block configuration was not found.",
	}
	ServerSideEncryptionConfigurationNotFound = APIError{
		"ServerSideEncryptionConfigurationNotFoundError", http.StatusNotFound,
		"The server side encryption configuration was not found.",
	}

	IllegalVersioningConfiguration = APIError{
		"IllegalVersioningConfigurationException", http.StatusBadRequest,
		"The versioning configuration specified in the request is invalid.",
	}
	OwnershipControlsNotFound = APIError{
		"OwnershipControlsNotFoundError", http.StatusNotFound,
		"The bucket ownership controls were not found.",
	}
	BucketAlreadyExists     = APIError{"BucketAlreadyExists", http.StatusConflict, "The requested bucket name is not available."}
	BucketAlreadyOwnedByYou = APIError{"BucketAlreadyOwnedByYou", http.StatusConflict, "The bucket you tried to create already exists and you own it."}
	BucketNotEmpty          = APIError{"BucketNotEmpty", http.StatusConflict, "The bucket you tried to delete is not empty."}
	InvalidBucketName       = APIError{"InvalidBucketName", http.StatusBadRequest, "The specified bucket is not valid."}
	InvalidURI              = APIError{"InvalidURI", http.StatusBadRequest, "Couldn't parse the specified URI."}
	InvalidArgument         = APIError{"InvalidArgument", http.StatusBadRequest, "Invalid Argument."}
	InvalidRequest          = APIError{codeInvalidRequest, http.StatusBadRequest, "Invalid Request."}
	CustomerKeyMismatch     = APIError{
		codeInvalidRequest, http.StatusBadRequest,
		"The object was stored using a form of Server Side Encryption. The correct parameters must be provided to retrieve the object.",
	}
	MalformedPOSTRequest = APIError{
		"MalformedPOSTRequest", http.StatusBadRequest,
		"The body of your POST request is not well-formed multipart/form-data.",
	}
	MalformedXML            = APIError{"MalformedXML", http.StatusBadRequest, "The XML you provided was not well-formed or did not validate against our published schema"}
	MissingContentLength    = APIError{"MissingContentLength", http.StatusLengthRequired, "You must provide the Content-Length HTTP header."}
	InvalidPart             = APIError{"InvalidPart", http.StatusBadRequest, "One or more of the specified parts could not be found."}
	InvalidPartOrder        = APIError{"InvalidPartOrder", http.StatusBadRequest, "The list of parts was not in ascending order. Parts must be ordered by part number."}
	InvalidPartNumber       = APIError{"InvalidPartNumber", http.StatusBadRequest, "The requested partnumber is not satisfiable."}
	EntityTooSmall          = APIError{"EntityTooSmall", http.StatusBadRequest, "Your proposed upload is smaller than the minimum allowed object size."}
	EntityTooLarge          = APIError{"EntityTooLarge", http.StatusBadRequest, "Your proposed upload exceeds the maximum allowed object size."}
	InvalidRange            = APIError{"InvalidRange", http.StatusRequestedRangeNotSatisfiable, "The requested range is not satisfiable"}
	InvalidTag              = APIError{"InvalidTag", http.StatusBadRequest, "The tag provided was not a valid tag."}
	InvalidDigest           = APIError{"InvalidDigest", http.StatusBadRequest, "The Content-MD5 you specified is not valid."}
	BadDigest               = APIError{"BadDigest", http.StatusBadRequest, "The Content-MD5 you specified did not match what we received."}
	PreconditionFailed      = APIError{"PreconditionFailed", http.StatusPreconditionFailed, "At least one of the pre-conditions you specified did not hold"}
	NotModified             = APIError{"NotModified", http.StatusNotModified, ""}
	AccessDenied            = APIError{"AccessDenied", http.StatusForbidden, "Access Denied"}
	QuotaExceeded           = APIError{"QuotaExceeded", http.StatusForbidden, "The quota of the bucket or its owner would be exceeded."}
	SignatureDoesNotMatch   = APIError{"SignatureDoesNotMatch", http.StatusForbidden, "The request signature we calculated does not match the signature you provided."}
	InvalidAccessKeyID      = APIError{"InvalidAccessKeyId", http.StatusForbidden, "The AWS access key Id you provided does not exist in our records."}
	RequestTimeTooSkewed    = APIError{"RequestTimeTooSkewed", http.StatusForbidden, "The difference between the request time and the current time is too large."}
	AuthHeaderMalformed     = APIError{"AuthorizationHeaderMalformed", http.StatusBadRequest, "The authorization header that you provided is not valid."}
	MissingSecurityHeader   = APIError{"MissingSecurityHeader", http.StatusBadRequest, "Your request is missing a required header."}
	ExpiredPresignedRequest = APIError{"AccessDenied", http.StatusForbidden, "Request has expired."}
	MethodNotAllowed        = APIError{"MethodNotAllowed", http.StatusMethodNotAllowed, "The specified method is not allowed against this resource."}
	NotImplemented          = APIError{"NotImplemented", http.StatusNotImplemented, "A header or operation you provided implies functionality that is not implemented."}
	MissingRequestBody      = APIError{"MissingRequestBodyError", http.StatusBadRequest, "Request body is empty."}
	MissingOriginHeader     = APIError{codeInvalidRequest, http.StatusBadRequest, "Insufficient information. Origin request header needed."}
	MissingRequestMethod    = APIError{codeInvalidRequest, http.StatusBadRequest, "Insufficient information. Access-Control-Request-Method header needed."}
	InternalError           = APIError{"InternalError", http.StatusInternalServerError, "We encountered an internal error. Please try again."}
	ServiceUnavailable      = APIError{"ServiceUnavailable", http.StatusServiceUnavailable, "Service is unable to handle request."}
)

// errorResponse is the standard S3 <Error> document. AWS S3 puts no
// <Resource> in it; the bucket, key or upload concerned are named by the
// code's own elements.
type errorResponse struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
	Detail
	RequestID string `xml:"RequestId"`
}

// FromError maps a Go error to an APIError, resolving the fs.Err* sentinels and
// falling back to InternalError for anything unrecognized.
func FromError(err error) APIError {
	switch {
	case err == nil:
		return InternalError
	case errors.Is(err, fs.ErrBucketNotFound):
		return NoSuchBucket
	case errors.Is(err, fs.ErrVersionNotFound):
		return NoSuchVersion
	case errors.Is(err, fs.ErrObjectNotFound):
		return NoSuchKey
	case errors.Is(err, fs.ErrUploadNotFound):
		return NoSuchUpload
	case errors.Is(err, fs.ErrBucketOwnedBySomeoneElse):
		// The name is taken by another principal: S3 reports the name as
		// unavailable, not as something the caller already owns.
		return BucketAlreadyExists
	case errors.Is(err, fs.ErrBucketAlreadyExists):
		return BucketAlreadyOwnedByYou
	case errors.Is(err, fs.ErrBucketNotEmpty):
		return BucketNotEmpty
	case errors.Is(err, fs.ErrInvalidBucketName):
		return InvalidBucketName
	case errors.Is(err, fs.ErrInvalidKey):
		// S3 reports keys it cannot address as an unparseable URI.
		return InvalidURI
	case errors.Is(err, fs.ErrPreconditionFailed):
		return PreconditionFailed
	case errors.Is(err, fs.ErrInvalidPart):
		return InvalidPart
	case errors.Is(err, fs.ErrInvalidPartOrder):
		return InvalidPartOrder
	case errors.Is(err, fs.ErrInvalidPartNumber):
		// AWS rejects out-of-range part numbers on UploadPart with
		// InvalidArgument (InvalidPartNumber is reserved for GetObject).
		return InvalidArgument
	case errors.Is(err, fs.ErrEntityTooSmall):
		return EntityTooSmall
	case errors.Is(err, fs.ErrMethodNotAllowedOnDeleteMarker):
		return MethodNotAllowed
	case errors.Is(err, fs.ErrAccessDenied):
		return AccessDenied
	case errors.Is(err, fs.ErrQuotaExceeded):
		// S3 has no quotas; this is Ceph RGW's code, which clients that
		// know RGW recognize.
		return QuotaExceeded
	case errors.Is(err, fs.ErrCustomerKeyMismatch):
		return CustomerKeyMismatch
	case errors.Is(err, fs.ErrInvalidTag):
		return InvalidTag
	case errors.Is(err, fs.ErrInvalidDigest):
		return InvalidDigest
	case errors.Is(err, fs.ErrBadDigest):
		return BadDigest
	case errors.Is(err, fs.ErrIntegrity):
		// Server-side corruption: the object is damaged, so surface a 500
		// rather than serve bad bytes.
		return InternalError
	case errors.Is(err, fs.ErrUnsupportedOperation):
		return NotImplemented
	case errors.Is(err, fs.ErrUnavailable):
		return ServiceUnavailable
	default:
		return InternalError
	}
}

// Write renders err as an S3 error response, mapping it through FromError
// and carrying any Detail attached with WithDetail.
func Write(w http.ResponseWriter, r *http.Request, err error) {
	WriteAPI(w, r, FromError(err), DetailOf(err))
}

// detailOf merges details, first wins, over what the request tells.
func detailOf(r *http.Request, api APIError, details []Detail) Detail {
	var d Detail

	for _, o := range details {
		d = d.merge(o)
	}

	return d.merge(fromRequest(r, api))
}

// fromRequest is the Detail the request itself tells for api's code: the
// bucket, key or upload it addresses, and its method. A handler that knows
// better (a copy's source key, say) sets the element itself.
func fromRequest(r *http.Request, api APIError) Detail {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	switch api.Code {
	case NoSuchBucket.Code, NoSuchLifecycleConfiguration.Code, NoSuchCORSConfiguration.Code,
		BucketNotEmpty.Code, BucketAlreadyExists.Code, BucketAlreadyOwnedByYou.Code:
		return Detail{BucketName: bucket}
	case NoSuchKey.Code:
		return Detail{Key: key}
	case NoSuchVersion.Code:
		return Detail{Key: key, VersionID: r.URL.Query().Get("versionId")}
	case NoSuchUpload.Code:
		return Detail{UploadID: r.URL.Query().Get("uploadId")}
	case MethodNotAllowed.Code:
		d := Detail{Method: r.Method}

		switch {
		case key != "":
			d.ResourceType = "OBJECT"
		case bucket != "":
			d.ResourceType = "BUCKET"
		}

		return d
	case PreconditionFailed.Code:
		// The header that failed, when the request carries only one that
		// could have.
		for _, h := range []string{"If-Match", "If-None-Match", "If-Unmodified-Since"} {
			if r.Header.Get(h) != "" {
				return Detail{Condition: h}
			}
		}
	}

	return Detail{}
}

// WriteAPI renders a specific APIError, with the elements of details and
// those the request itself tells (fromRequest), details first. It writes no
// body for HEAD requests (S3 returns bare status codes there) and never
// panics: if XML encoding fails it still emits the status code. The
// x-amz-request-id header, if already set (e.g. by middleware), is echoed
// into the <RequestId> element.
func WriteAPI(w http.ResponseWriter, r *http.Request, api APIError, details ...Detail) {
	header := w.Header()
	requestID := header.Get("x-amz-request-id")

	if r.Method == http.MethodHead {
		// A response to HEAD must not carry a body.
		header.Del("Content-Type")
		w.WriteHeader(api.HTTPStatus)

		return
	}

	body, marshalErr := xml.Marshal(errorResponse{
		Code:      api.Code,
		Message:   api.Message,
		Detail:    detailOf(r, api, details),
		RequestID: requestID,
	})

	n := 0
	if marshalErr == nil {
		n = len(xml.Header) + len(body)
	}

	// The length is declared so the response is complete as soon as it is
	// written: drainBody sends it before reading a refused body, and a client
	// that waits for the response before sending must not wait on its end.
	header.Set("Content-Type", "application/xml")
	header.Set("Content-Length", strconv.Itoa(n))
	w.WriteHeader(api.HTTPStatus)

	if marshalErr != nil {
		// Non-panicking fallback: the status line already conveys the error.
		return
	}

	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}
