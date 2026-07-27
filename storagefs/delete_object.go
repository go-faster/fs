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
	return s.DeleteObjectIf(ctx, bucket, key, fs.Conditions{})
}

// DeleteObjectIf implements fs.ConditionalDeleter: it deletes the object only
// while cond holds. The condition is evaluated under putMu, the same lock that
// serializes writes to the key, so no writer can slip between the check and the
// unlink.
//
// NB: bucket and key are already sanitized.
func (s *Storage) DeleteObjectIf(_ context.Context, bucket, key string, cond fs.Conditions) error {
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return fs.ErrBucketNotFound
	}

	objectPath := filepath.Join(bucketPath, toOSPath(key))

	if !cond.IsZero() {
		s.putMu.Lock()
		defer s.putMu.Unlock()

		state, err := s.currentObjectState(bucket, key, objectPath)
		if err != nil {
			return err
		}

		if err := cond.CheckDelete(state); err != nil {
			return err
		}
	}

	if err := os.Remove(objectPath); err != nil {
		if os.IsNotExist(err) {
			return fs.ErrObjectNotFound
		}

		return errors.Wrap(err, "delete object")
	}

	s.deleteSidecar(bucket, key)

	// Prune the now-empty parent directories left behind by a nested key, up
	// to (but not including) the bucket root, so a bucket whose objects have
	// all been deleted becomes genuinely empty and can be removed.
	pruneEmptyDirs(filepath.Dir(objectPath), bucketPath)

	return nil
}

// pruneEmptyDirs removes dir and its now-empty ancestors, climbing until stop
// (exclusive). It stops at the first directory that is not empty (os.Remove
// fails) or that no longer exists, and never touches stop or anything above it.
func pruneEmptyDirs(dir, stop string) {
	for len(dir) > len(stop) && dir != stop {
		if err := os.Remove(dir); err != nil {
			return
		}

		dir = filepath.Dir(dir)
	}
}
