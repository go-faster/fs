package storagefs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

const defaultDirPermissions = 0750

func (s *Storage) CreateBucket(ctx context.Context, bucket string) error {
	return s.CreateBucketOwned(ctx, bucket, fs.Owner{})
}

// CreateBucketOwned implements fs.BucketOwnership. The owner is recorded after
// the directory exists, so a crash in between leaves an unowned bucket — which
// reads as "created before ownership", the same safe fallback older data gets.
func (s *Storage) CreateBucketOwned(_ context.Context, bucket string, owner fs.Owner) error {
	bucketPath := filepath.Join(s.root, bucket)
	if err := os.Mkdir(bucketPath, defaultDirPermissions); err != nil {
		if os.IsExist(err) {
			return errors.Wrapf(fs.ErrBucketAlreadyExists, "bucket %q", bucket)
		}

		return fmt.Errorf("failed to create bucket: %w", err)
	}

	if owner.IsZero() {
		return nil
	}

	meta := s.readBucketMeta(bucket)
	meta.Owner = owner

	if err := s.writeBucketMeta(bucket, meta); err != nil {
		return errors.Wrap(err, "record bucket owner")
	}

	return nil
}

// BucketOwner implements fs.BucketOwnership.
func (s *Storage) BucketOwner(_ context.Context, bucket string) (fs.Owner, error) {
	if _, err := os.Stat(filepath.Join(s.root, bucket)); os.IsNotExist(err) {
		return fs.Owner{}, fs.ErrBucketNotFound
	}

	return s.readBucketMeta(bucket).Owner, nil
}
