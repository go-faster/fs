package fs

import "github.com/go-faster/errors"

var (
	ErrBucketNotFound       = errors.New("bucket not found")
	ErrBucketAlreadyExists  = errors.New("bucket already exists")
	ErrBucketNotEmpty       = errors.New("bucket not empty")
	ErrObjectNotFound       = errors.New("object not found")
	ErrUploadNotFound       = errors.New("upload not found")
	ErrInvalidBucketName    = errors.New("invalid bucket name")
	ErrUnsupportedOperation = errors.New("unsupported operation")
	ErrPreconditionFailed   = errors.New("precondition failed")
	// ErrAccessDenied reports a request the caller is not authorized to make.
	ErrAccessDenied = errors.New("access denied")
	// ErrMethodNotAllowedOnDeleteMarker reports a read addressed at a delete
	// marker. A marker exists but has no content, which S3 distinguishes from
	// a missing key: the caller asked for something that is there and cannot
	// be read, not for something that is not there.
	ErrMethodNotAllowedOnDeleteMarker = errors.New("method not allowed on a delete marker")
	// ErrBucketOwnedBySomeoneElse reports a create against a bucket name that
	// another principal already holds.
	ErrBucketOwnedBySomeoneElse = errors.New("bucket owned by someone else")
	// ErrInvalidDigest reports a Content-MD5 the server could not parse: not
	// base64, or not 16 bytes once decoded.
	ErrInvalidDigest = errors.New("invalid content digest")
	// ErrBadDigest reports a Content-MD5 that parsed but does not match the
	// bytes received. The object is not stored.
	ErrBadDigest = errors.New("content digest mismatch")

	// ErrInvalidKey reports an object key the server cannot address: not valid
	// UTF-8, over the 1024-byte limit, or carrying path elements that would
	// escape the bucket.
	ErrInvalidKey = errors.New("invalid object key")
	// ErrInvalidPart reports that a part referenced by CompleteMultipartUpload
	// was never uploaded or its ETag does not match.
	ErrInvalidPart = errors.New("invalid part")
	// ErrInvalidPartOrder reports that the CompleteMultipartUpload part list is
	// not in strictly ascending part-number order.
	ErrInvalidPartOrder = errors.New("invalid part order")
	// ErrInvalidPartNumber reports a part number outside the valid 1..10000 range.
	ErrInvalidPartNumber = errors.New("invalid part number")
	// ErrEntityTooSmall reports a non-last multipart part smaller than the 5 MiB
	// minimum.
	ErrEntityTooSmall = errors.New("entity too small")
	// ErrInvalidTag reports an object tag set violating the S3 limits
	// (at most 10 tags, unique keys, key ≤ 128 chars, value ≤ 256 chars).
	ErrInvalidTag = errors.New("invalid tag")

	// ErrIntegrity reports that an object's stored content does not match its
	// recorded checksum (bit-rot / corruption detected on read).
	ErrIntegrity = errors.New("object integrity check failed")
)
