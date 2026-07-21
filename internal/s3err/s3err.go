// Package s3err renders S3-compatible error responses.
//
// AWS SDKs parse the XML <Error> document body and raise typed exceptions from
// its <Code> element, so wire-correct error responses (not a bespoke JSON
// shape) are what makes real clients work against the server.
package s3err

import (
	"encoding/xml"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// APIError describes an S3 error: a stable wire code, its HTTP status, and a
// default human-readable message.
type APIError struct {
	Code       string
	HTTPStatus int
	Message    string
}

// The error table. These are the codes real SDK/client paths depend on; each
// maps to exactly one HTTP status and a default message.
var (
	NoSuchBucket            = APIError{"NoSuchBucket", http.StatusNotFound, "The specified bucket does not exist."}
	NoSuchKey               = APIError{"NoSuchKey", http.StatusNotFound, "The specified key does not exist."}
	NoSuchUpload            = APIError{"NoSuchUpload", http.StatusNotFound, "The specified multipart upload does not exist."}
	NoSuchBucketPolicy      = APIError{"NoSuchBucketPolicy", http.StatusNotFound, "The bucket policy does not exist."}
	BucketAlreadyExists     = APIError{"BucketAlreadyExists", http.StatusConflict, "The requested bucket name is not available."}
	BucketAlreadyOwnedByYou = APIError{"BucketAlreadyOwnedByYou", http.StatusConflict, "The bucket you tried to create already exists and you own it."}
	BucketNotEmpty          = APIError{"BucketNotEmpty", http.StatusConflict, "The bucket you tried to delete is not empty."}
	InvalidBucketName       = APIError{"InvalidBucketName", http.StatusBadRequest, "The specified bucket is not valid."}
	InvalidArgument         = APIError{"InvalidArgument", http.StatusBadRequest, "Invalid Argument."}
	InvalidRequest          = APIError{"InvalidRequest", http.StatusBadRequest, "Invalid Request."}
	MalformedXML            = APIError{"MalformedXML", http.StatusBadRequest, "The XML you provided was not well-formed or did not validate against our published schema."}
	MissingContentLength    = APIError{"MissingContentLength", http.StatusLengthRequired, "You must provide the Content-Length HTTP header."}
	InvalidPart             = APIError{"InvalidPart", http.StatusBadRequest, "One or more of the specified parts could not be found."}
	InvalidPartOrder        = APIError{"InvalidPartOrder", http.StatusBadRequest, "The list of parts was not in ascending order. Parts must be ordered by part number."}
	InvalidPartNumber       = APIError{"InvalidPartNumber", http.StatusBadRequest, "The requested partnumber is not satisfiable."}
	EntityTooSmall          = APIError{"EntityTooSmall", http.StatusBadRequest, "Your proposed upload is smaller than the minimum allowed object size."}
	EntityTooLarge          = APIError{"EntityTooLarge", http.StatusBadRequest, "Your proposed upload exceeds the maximum allowed object size."}
	InvalidRange            = APIError{"InvalidRange", http.StatusRequestedRangeNotSatisfiable, "The requested range is not satisfiable."}
	InvalidTag              = APIError{"InvalidTag", http.StatusBadRequest, "The tag provided was not a valid tag."}
	PreconditionFailed      = APIError{"PreconditionFailed", http.StatusPreconditionFailed, "At least one of the preconditions you specified did not hold."}
	NotModified             = APIError{"NotModified", http.StatusNotModified, ""}
	AccessDenied            = APIError{"AccessDenied", http.StatusForbidden, "Access Denied."}
	SignatureDoesNotMatch   = APIError{"SignatureDoesNotMatch", http.StatusForbidden, "The request signature we calculated does not match the signature you provided."}
	InvalidAccessKeyID      = APIError{"InvalidAccessKeyId", http.StatusForbidden, "The AWS access key Id you provided does not exist in our records."}
	RequestTimeTooSkewed    = APIError{"RequestTimeTooSkewed", http.StatusForbidden, "The difference between the request time and the current time is too large."}
	AuthHeaderMalformed     = APIError{"AuthorizationHeaderMalformed", http.StatusBadRequest, "The authorization header that you provided is not valid."}
	MissingSecurityHeader   = APIError{"MissingSecurityHeader", http.StatusBadRequest, "Your request is missing a required header."}
	ExpiredPresignedRequest = APIError{"AccessDenied", http.StatusForbidden, "Request has expired."}
	MethodNotAllowed        = APIError{"MethodNotAllowed", http.StatusMethodNotAllowed, "The specified method is not allowed against this resource."}
	NotImplemented          = APIError{"NotImplemented", http.StatusNotImplemented, "A header or operation you provided implies functionality that is not implemented."}
	MissingRequestBody      = APIError{"MissingRequestBodyError", http.StatusBadRequest, "Request body is empty."}
	InternalError           = APIError{"InternalError", http.StatusInternalServerError, "We encountered an internal error. Please try again."}
)

// errorResponse is the standard S3 <Error> document.
type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

// FromError maps a Go error to an APIError, resolving the fs.Err* sentinels and
// falling back to InternalError for anything unrecognized.
func FromError(err error) APIError {
	switch {
	case err == nil:
		return InternalError
	case errors.Is(err, fs.ErrBucketNotFound):
		return NoSuchBucket
	case errors.Is(err, fs.ErrObjectNotFound):
		return NoSuchKey
	case errors.Is(err, fs.ErrUploadNotFound):
		return NoSuchUpload
	case errors.Is(err, fs.ErrBucketAlreadyExists):
		return BucketAlreadyOwnedByYou
	case errors.Is(err, fs.ErrBucketNotEmpty):
		return BucketNotEmpty
	case errors.Is(err, fs.ErrInvalidBucketName):
		return InvalidBucketName
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
	case errors.Is(err, fs.ErrInvalidTag):
		return InvalidTag
	case errors.Is(err, fs.ErrIntegrity):
		// Server-side corruption: the object is damaged, so surface a 500
		// rather than serve bad bytes.
		return InternalError
	case errors.Is(err, fs.ErrUnsupportedOperation):
		return NotImplemented
	default:
		return InternalError
	}
}

// Write renders err as an S3 error response, mapping it through FromError.
func Write(w http.ResponseWriter, r *http.Request, err error) {
	WriteAPI(w, r, FromError(err))
}

// WriteAPI renders a specific APIError. It writes no body for HEAD requests (S3
// returns bare status codes there) and never panics: if XML encoding fails it
// still emits the status code. The x-amz-request-id header, if already set
// (e.g. by middleware), is echoed into the <RequestId> element.
func WriteAPI(w http.ResponseWriter, r *http.Request, api APIError) {
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
		Resource:  r.URL.Path,
		RequestID: requestID,
	})

	header.Set("Content-Type", "application/xml")
	w.WriteHeader(api.HTTPStatus)

	if marshalErr != nil {
		// Non-panicking fallback: the status line already conveys the error.
		return
	}

	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}
