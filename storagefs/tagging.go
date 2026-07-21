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

	info, err := os.Stat(filepath.Join(bucketPath, toOSPath(key)))
	if os.IsNotExist(err) || (err == nil && info.IsDir()) {
		return fs.ErrObjectNotFound
	}

	return err
}

func (s *Storage) GetObjectTagging(_ context.Context, bucket, key string) ([]fs.Tag, error) {
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
func (s *Storage) updateTags(bucket, key string, tags []fs.Tag) error {
	if err := s.statObject(bucket, key); err != nil {
		return err
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	sc, err := s.readSidecar(bucket, key)
	if err != nil {
		return err
	}

	if sc == nil {
		sc = newSidecar(key, "", "", fs.ObjectMetadata{}, nil, fs.ACLPrivate)
	}

	sc.Tags = tags

	return s.writeSidecar(bucket, sc)
}
