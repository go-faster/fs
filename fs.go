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

// Object represents an S3 object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

// ObjectMetadata holds the user-controlled metadata stored with an object:
// the standard HTTP representation headers plus x-amz-meta-* pairs.
type ObjectMetadata struct {
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	// UserMetadata holds x-amz-meta-* pairs, keyed by the lowercase name
	// without the prefix (e.g. "color" for x-amz-meta-color).
	UserMetadata map[string]string
}

// IsZero reports whether no metadata field is set.
func (m ObjectMetadata) IsZero() bool {
	return m.ContentType == "" && m.CacheControl == "" && m.ContentDisposition == "" &&
		m.ContentEncoding == "" && len(m.UserMetadata) == 0
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

	// IfNoneMatch and IfMatch carry the raw conditional-write header values
	// (e.g. "*" or a quoted ETag list). When set, the storage backend must
	// evaluate them atomically with the write — see PreconditionFailed — so
	// concurrent conditional PUTs resolve to a single winner. Empty means no
	// condition.
	IfNoneMatch string
	IfMatch     string
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
}

// Part represents a part of a multipart upload.
type Part struct {
	PartNumber   int
	ETag         string
	Size         int64
	LastModified time.Time
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
}

// CompleteMultipartUploadResponse represents the response for completing multipart upload.
type CompleteMultipartUploadResponse struct {
	Location string
	Bucket   string
	Key      string
	ETag     string
}
