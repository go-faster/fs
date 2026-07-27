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

		// os.Remove fails on any non-empty directory, but a bucket holding only
		// key directories with nothing in them holds no objects: a refused write
		// creates the directory before it fails, and a delete can leave the
		// chain behind. Emptiness is about objects, so look for content rather
		// than for directory entries.
		empty, cerr := bucketHasNoObjects(bucketPath)
		if cerr != nil {
			return errors.Wrap(cerr, "inspect bucket")
		}

		if !empty {
			return fs.ErrBucketNotEmpty
		}

		if err := os.RemoveAll(bucketPath); err != nil {
			return errors.Wrap(err, "delete bucket")
		}
	}

	s.deleteBucketMeta(bucket)

	return nil
}

// bucketHasNoObjects reports whether a bucket directory contains no object
// content, ignoring the empty key directories a refused write or a delete can
// leave behind.
func bucketHasNoObjects(bucketPath string) (bool, error) {
	empty := true

	err := filepath.Walk(bucketPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			empty = false
			return filepath.SkipAll
		}

		return nil
	})
	if err != nil {
		return false, err
	}

	return empty, nil
}
