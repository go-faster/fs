package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (s *Storage) GetObject(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
	objectPath := filepath.Join(s.root, bucket, toOSPath(key))

	// Check if bucket exists
	bucketPath := filepath.Join(s.root, bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	// #nosec G304 -- objectPath is constructed from validated bucket and key.
	f, err := os.Open(objectPath)
	if os.IsNotExist(err) {
		return nil, fs.ErrObjectNotFound
	}

	if err != nil {
		return nil, errors.Wrap(err, "open object")
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, "stat object")
	}

	// Verify-on-read: recompute and check the checksum before serving so corrupt
	// content is never returned (opt-in; costs an extra full read).
	if s.verifyReads {
		if err := s.verifyContent(bucket, key, objectPath); err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	resp := &fs.GetObjectResponse{
		Reader:       f,
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}

	// The sidecar carries the stored ETag and metadata; files without one
	// (pre-sidecar data directories) fall back to recompute-on-read.
	sc, err := s.readSidecar(bucket, key)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	if sc != nil {
		resp.ETag = sc.ETag
		resp.Metadata = sc.metadata()
	}

	if resp.ETag == "" {
		etag, err := s.etagFor(objectPath, info)
		if err != nil {
			_ = f.Close()
			return nil, errors.Wrap(err, "etag")
		}

		resp.ETag = etag
	}

	return resp, nil
}
