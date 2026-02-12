package storagefs

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// ListObjects lists all objects in bucket by prefix.
//
// NB: bucket and prefix are already sanitized.
func (s *Storage) ListObjects(ctx context.Context, bucket, prefix string) ([]fs.Object, error) {
	bucketPath := filepath.Join(s.root, bucket)

	var objects []fs.Object

	err := filepath.Walk(bucketPath, func(path string, info os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			// Stop walking on context done.
			return ctx.Err()
		default:
		}

		if os.IsNotExist(err) {
			return fs.ErrBucketNotFound
		}

		if err != nil {
			return errors.Wrap(err, "walk objects")
		}

		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(bucketPath, path)
		if err != nil {
			return errors.Wrap(err, "determine relative path")
		}

		// Convert to forward slashes for S3 compatibility.
		key := filepath.ToSlash(relPath)

		if prefix == "" || strings.HasPrefix(key, prefix) {
			objects = append(objects, fs.Object{
				Key:          key,
				Size:         info.Size(),
				LastModified: info.ModTime(),
			})
		}

		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "list objects")
	}

	return objects, nil
}
