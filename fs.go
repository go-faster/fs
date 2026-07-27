// Package fs is a S3-compatible storage server implementation.
package fs

import (
	"io"
	"time"
)

// Bucket represents an S3 bucket.
type Bucket struct {
	Name         string
	CreationDate time.Time
}

// Owner identifies the S3 principal that owns a bucket or object. It is
// recorded when the object is written and reported in ACL and listing
// responses; clients compare it to decide what a caller owns, so it must not
// change when a different credential reads the object.
type Owner struct {
	// ID is the canonical user ID.
	ID string
	// DisplayName is the human-readable owner name.
	DisplayName string
}

// IsZero reports whether no owner was recorded (objects written before owners
// were modeled, or by an unauthenticated server).
func (o Owner) IsZero() bool {
	return o.ID == "" && o.DisplayName == ""
}

// Object represents an S3 object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	// Owner is the principal that wrote the object; zero when unrecorded.
	Owner Owner
}

// ObjectMetadata holds the user-controlled metadata stored with an object:
// the standard HTTP representation headers plus x-amz-meta-* pairs.
type ObjectMetadata struct {
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	// Expires is the raw Expires header value, stored and returned verbatim.
	// S3 keeps whatever the client sent rather than reformatting it, and a
	// client that round-trips the header expects its own bytes back.
	Expires string
	// UserMetadata holds x-amz-meta-* pairs, keyed by the lowercase name
	// without the prefix (e.g. "color" for x-amz-meta-color).
	UserMetadata map[string]string
}

// IsZero reports whether no metadata field is set.
func (m ObjectMetadata) IsZero() bool {
	return m.ContentType == "" && m.CacheControl == "" && m.ContentDisposition == "" &&
		m.ContentEncoding == "" && m.Expires == "" && len(m.UserMetadata) == 0
}

// Tag is a single object tag.
type Tag struct {
	Key   string
	Value string
}

type PutObjectRequest struct {
	Reader   io.Reader
	Bucket   string
	Key      string
	Size     int64
	Metadata ObjectMetadata
	Tags     []Tag
	// ACL is the canned access-control level for the object (default
	// ACLPrivate). Governs anonymous access only.
	ACL ACL
	// Owner is the principal writing the object, recorded with it and reported
	// in ACL and listing responses. Zero leaves the object unowned.
	Owner Owner

	// IfNoneMatch and IfMatch carry the raw conditional-write header values
	// (e.g. "*" or a quoted ETag list). When set, the storage backend must
	// evaluate them atomically with the write — see PreconditionFailed — so
	// concurrent conditional PUTs resolve to a single winner. Empty means no
	// condition.
	IfNoneMatch string
	IfMatch     string

	// ContentMD5 is the hex-encoded MD5 the client claims the body has
	// (decoded from the base64 Content-MD5 header). When set, the backend must
	// compare it against what it actually received and refuse the write with
	// ErrBadDigest before the object becomes visible — verifying afterwards
	// would leave corrupt content readable in the window between.
	ContentMD5 string
}

// PutObjectResponse reports the stored object's ETag.
type PutObjectResponse struct {
	ETag string
}

// GetObjectResponse represents the response for GetObject operation.
type GetObjectResponse struct {
	Reader       io.ReadCloser
	Size         int64
	LastModified time.Time
	ETag         string
	Metadata     ObjectMetadata
	// TagCount is how many tags the object carries, reported on GET and HEAD
	// as x-amz-tagging-count. Backends fill it from metadata they already read;
	// zero means untagged (the header is then omitted).
	TagCount int
}

// MultipartUpload represents an in-progress multipart upload.
type MultipartUpload struct {
	UploadID  string
	Bucket    string
	Key       string
	Initiated time.Time
}

// CreateMultipartUploadRequest represents a request to start a multipart
// upload. Metadata and tags are applied to the object at completion.
type CreateMultipartUploadRequest struct {
	Bucket   string
	Key      string
	Metadata ObjectMetadata
	Tags     []Tag
	ACL      ACL
	// Owner is the principal starting the upload; it owns the completed object.
	Owner Owner
}

// Part represents a part of a multipart upload.
type Part struct {
	PartNumber   int
	ETag         string
	Size         int64
	LastModified time.Time
}

// ObjectPart is one part of a *completed* multipart object, retained after the
// upload finishes so the object can still be read and described a part at a
// time (GetObjectAttributes, ?partNumber=N).
type ObjectPart struct {
	PartNumber int
	Size       int64
	ETag       string
}

// ObjectAttributes describes an object without opening its body: what
// GetObjectAttributes reports, plus the part layout when the object was
// assembled from a multipart upload (nil for a single PUT).
type ObjectAttributes struct {
	ETag         string
	Size         int64
	LastModified time.Time
	// UploadID names the multipart upload this object was completed from, when
	// it was. It is what lets a retried CompleteMultipartUpload be recognized
	// as a retry rather than answered with NoSuchUpload; empty for a single
	// PUT.
	UploadID string
	// Parts is the completed part layout in ascending part-number order, or
	// nil when the object was written by a single PUT.
	Parts []ObjectPart
}

// PartRange returns the byte range covered by part number n (1-based) and
// whether it exists. For an object with no recorded parts, part 1 is the whole
// object, which is what S3 reports for a single PUT.
func (a *ObjectAttributes) PartRange(n int) (offset, length int64, ok bool) {
	if len(a.Parts) == 0 {
		if n != 1 {
			return 0, 0, false
		}

		return 0, a.Size, true
	}

	for _, p := range a.Parts {
		if p.PartNumber == n {
			return offset, p.Size, true
		}

		offset += p.Size
	}

	return 0, 0, false
}

// PartsCount reports how many parts the object was assembled from; zero when
// it was written by a single PUT.
func (a *ObjectAttributes) PartsCount() int {
	return len(a.Parts)
}

// UploadPartRequest represents a request to upload a part.
type UploadPartRequest struct {
	Bucket     string
	Key        string
	UploadID   string
	PartNumber int
	Reader     io.Reader
	Size       int64
}

// CompletedPart represents a completed part for completing multipart upload.
type CompletedPart struct {
	PartNumber int
	ETag       string
}

// CompleteMultipartUploadRequest represents a request to complete multipart upload.
type CompleteMultipartUploadRequest struct {
	Bucket   string
	Key      string
	UploadID string
	Parts    []CompletedPart
	// Conditions are the conditional-write headers carried by the completion
	// request. A backend that supports them must evaluate them atomically with
	// the write, exactly as it does for a conditional PutObject; the zero value
	// imposes no condition.
	Conditions Conditions
}

// CompleteMultipartUploadResponse represents the response for completing multipart upload.
type CompleteMultipartUploadResponse struct {
	Location string
	Bucket   string
	Key      string
	ETag     string
}
