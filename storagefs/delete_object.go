package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// DeleteObject deletes the specified object from the bucket.
//
// NB: bucket and key are already sanitized.
func (s *Storage) DeleteObject(ctx context.Context, bucket, key string) error {
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return fs.ErrBucketNotFound
	}

	objectPath := filepath.Join(bucketPath, toOSPath(key))

	if err := os.Remove(objectPath); err != nil {
		if os.IsNotExist(err) {
			return fs.ErrObjectNotFound
		}

		return errors.Wrap(err, "delete object")
	}

	return nil
}
