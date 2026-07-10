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
	bucketPath := filepath.Join(s.root, bucket)
	if err := os.Mkdir(bucketPath, defaultDirPermissions); err != nil {
		if os.IsExist(err) {
			return errors.Wrapf(fs.ErrBucketAlreadyExists, "bucket %q", bucket)
		}

		return fmt.Errorf("failed to create bucket: %w", err)
	}

	return nil
}
