package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// ObjectAttributes implements fs.ObjectAttributer. The part layout comes from
// the sidecar written at multipart completion; an object without one (a single
// PUT, or a multipart object written before layouts were recorded) reports no
// parts, which reads as "one part covering the whole object".
func (s *Storage) ObjectAttributes(_ context.Context, bucket, key string) (*fs.ObjectAttributes, error) {
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	objectPath := filepath.Join(bucketPath, toOSPath(key))

	info, err := os.Stat(objectPath)
	if os.IsNotExist(err) {
		return nil, fs.ErrObjectNotFound
	}

	if err != nil {
		return nil, errors.Wrap(err, "stat object")
	}

	if info.IsDir() {
		return nil, fs.ErrObjectNotFound
	}

	etag, err := s.objectETag(bucket, key, objectPath, info)
	if err != nil {
		return nil, err
	}

	attrs := &fs.ObjectAttributes{
		ETag:         etag,
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}

	if sc, err := s.readSidecar(bucket, key); err == nil && sc != nil {
		attrs.Parts = sc.Parts
	}

	return attrs, nil
}
