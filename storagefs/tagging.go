package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/fs"
)

// statObject verifies the bucket and object exist.
func (s *Storage) statObject(bucket, key string) error {
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return fs.ErrBucketNotFound
	}

	info, err := os.Stat(filepath.Join(bucketPath, objectRelPath(key)))
	if os.IsNotExist(err) || (err == nil && info.IsDir()) {
		return fs.ErrObjectNotFound
	}

	return err
}

func (s *Storage) GetObjectTagging(_ context.Context, bucket, key string) ([]fs.Tag, error) {
	// On a versioned bucket the object is not at the plain path statObject
	// checks — it is under .versions, and its tags are in the current
	// version's sidecar, next to the bytes they describe. Without this, every
	// tag read on a versioned bucket answers NoSuchKey for an object that is
	// plainly there, and CopyObject fails with it, because a copy reads the
	// source's tags before writing them to the destination.
	switch current, deleted, err := s.currentVersion(bucket, key); {
	case err != nil:
		return nil, err
	case deleted:
		return nil, fs.ErrObjectNotFound
	case current != nil:
		return current.Tags, nil
	}

	if err := s.statObject(bucket, key); err != nil {
		return nil, err
	}

	sc, err := s.readSidecar(bucket, key)
	if err != nil || sc == nil {
		return nil, err
	}

	return sc.Tags, nil
}

func (s *Storage) PutObjectTagging(_ context.Context, bucket, key string, tags []fs.Tag) error {
	return s.updateTags(bucket, key, tags)
}

func (s *Storage) DeleteObjectTagging(_ context.Context, bucket, key string) error {
	return s.updateTags(bucket, key, nil)
}

// updateTags rewrites the object's sidecar with the new tag set, creating the
// sidecar (preserving nothing but the tags) for pre-sidecar objects.
//
// Tagging names no version, so it applies to the current one — S3's rule, and
// the only one that keeps a write and the read that follows it in agreement.
// Writing to the plain path instead would put the tags where no read on a
// versioned bucket looks, losing them silently.
func (s *Storage) updateTags(bucket, key string, tags []fs.Tag) error {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	switch current, deleted, err := s.currentVersion(bucket, key); {
	case err != nil:
		return err
	case deleted:
		return fs.ErrObjectNotFound
	case current != nil:
		current.Tags = tags
		return s.writeVersionSidecar(bucket, key, current)
	}

	if err := s.statObject(bucket, key); err != nil {
		return err
	}

	sc, err := s.readSidecar(bucket, key)
	if err != nil {
		return err
	}

	if sc == nil {
		sc = newSidecar(key, "", "", fs.ObjectMetadata{}, nil, fs.ACLPrivate, fs.Owner{})
	}

	sc.Tags = tags

	return s.writeSidecar(bucket, sc)
}
