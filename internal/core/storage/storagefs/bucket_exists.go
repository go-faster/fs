package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/fs"
)

func (s *Storage) BucketExists(_ context.Context, bucket string) (bool, error) {
	bucketPath := filepath.Join(s.root, bucket)

	info, err := os.Stat(bucketPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, err
	}

	if !info.IsDir() {
		return false, fs.ErrBucketNotFound
	}

	return true, nil
}
