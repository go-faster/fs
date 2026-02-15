package fs

import "github.com/go-faster/errors"

var (
	ErrBucketNotFound = errors.New("bucket not found")
	ErrObjectNotFound = errors.New("object not found")
	ErrUploadNotFound = errors.New("upload not found")
)
