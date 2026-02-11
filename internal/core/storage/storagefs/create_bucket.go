package storagefs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const defaultDirPermissions = 0750

func (s *Storage) CreateBucket(ctx context.Context, bucket string) error {
	bucketPath := filepath.Join(s.root, bucket)
	if err := os.MkdirAll(bucketPath, defaultDirPermissions); err != nil {
		return fmt.Errorf("failed to create bucket: %w", err)
	}

	return nil
}
