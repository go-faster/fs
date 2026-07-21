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
