package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"
)

// DeleteBucket deletes the specified bucket.
//
// NB: bucket is already sanitized.
func (s *Storage) DeleteBucket(ctx context.Context, bucket string) error {
	bucketPath := filepath.Join(s.root, bucket)

	if err := os.Remove(bucketPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("bucket not found")
		}
		return errors.Wrap(err, "delete bucket")
	}

	return nil
}
