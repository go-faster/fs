package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// DeleteBucket deletes the specified bucket.
//
// NB: bucket is already sanitized.
func (s *Storage) DeleteBucket(ctx context.Context, bucket string) error {
	bucketPath := filepath.Join(s.root, bucket)

	if err := os.Remove(bucketPath); err != nil {
		if os.IsNotExist(err) {
			return fs.ErrBucketNotFound
		}

		// os.Remove fails on a non-empty directory; report it as the S3
		// BucketNotEmpty condition rather than a generic internal error.
		if entries, rerr := os.ReadDir(bucketPath); rerr == nil && len(entries) > 0 {
			return fs.ErrBucketNotEmpty
		}

		return errors.Wrap(err, "delete bucket")
	}

	return nil
}
