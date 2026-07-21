package storagefs

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (s *Storage) PutObject(ctx context.Context, req *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
	bucketPath := filepath.Join(s.root, req.Bucket)
	if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
		return nil, fs.ErrBucketNotFound
	}

	objectPath := filepath.Join(bucketPath, toOSPath(req.Key))
	if err := os.MkdirAll(filepath.Dir(objectPath), defaultDirPermissions); err != nil {
		return nil, errors.Wrap(err, "create object directory")
	}

	// Stream to a temp file in the target directory while hashing, then rename
	// into place so a partially written object is never visible; the sidecar is
	// written after the object (sidecar-less files stay readable).
	tmp, err := os.CreateTemp(filepath.Dir(objectPath), ".tmp-put-*")
	if err != nil {
		return nil, errors.Wrap(err, "create temp object")
	}

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	hash := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.

	if _, err := io.Copy(io.MultiWriter(tmp, hash), req.Reader); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write object: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, errors.Wrap(err, "close object")
	}

	if err := os.Rename(tmp.Name(), objectPath); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, errors.Wrap(err, "rename object")
	}

	etag := hex.EncodeToString(hash.Sum(nil))

	if err := s.writeSidecar(req.Bucket, newSidecar(req.Key, etag, req.Metadata, req.Tags)); err != nil {
		return nil, err
	}

	return &fs.PutObjectResponse{ETag: etag}, nil
}
