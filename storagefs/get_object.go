package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (s *Storage) GetObject(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
	objectPath := filepath.Join(s.root, bucket, toOSPath(key))

	// Check if bucket exists
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	// #nosec G304 -- objectPath is constructed from validated bucket and key.
	f, err := os.Open(objectPath)
	if os.IsNotExist(err) {
		return nil, fs.ErrObjectNotFound
	}

	if err != nil {
		return nil, errors.Wrap(err, "open object")
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, "stat object")
	}

	return &fs.GetObjectResponse{
		Reader:       f,
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}, nil
}
