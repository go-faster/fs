package fs

import "github.com/go-faster/errors"

var (
	ErrBucketNotFound       = errors.New("bucket not found")
	ErrBucketAlreadyExists  = errors.New("bucket already exists")
	ErrObjectNotFound       = errors.New("object not found")
	ErrUploadNotFound       = errors.New("upload not found")
	ErrInvalidBucketName    = errors.New("invalid bucket name")
	ErrUnsupportedOperation = errors.New("unsupported operation")
	ErrPreconditionFailed   = errors.New("precondition failed")
)
