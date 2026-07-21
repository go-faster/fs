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

	// Stream to a staging temp file while hashing, then rename into place so a
	// partially written object is never visible in the bucket; the sidecar is
	// written after the object (sidecar-less files stay readable).
	tmp, err := s.newObjectTemp()
	if err != nil {
		return nil, err
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

	// Flush object data to stable storage before it becomes visible (per policy).
	if err := s.syncFile(tmp); err != nil {
		cleanup()
		return nil, err
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, errors.Wrap(err, "close object")
	}

	etag := hex.EncodeToString(hash.Sum(nil))

	// Finalize under putMu so the conditional-write check and the rename are
	// atomic against other writers to this key (the body is already on disk).
	s.putMu.Lock()
	defer s.putMu.Unlock()

	if req.IfNoneMatch != "" || req.IfMatch != "" {
		exists, currentETag, err := s.currentObjectState(req.Bucket, req.Key, objectPath)
		if err != nil {
			_ = os.Remove(tmp.Name())
			return nil, err
		}

		if req.PreconditionFailed(exists, currentETag) {
			_ = os.Remove(tmp.Name())
			return nil, fs.ErrPreconditionFailed
		}
	}

	if err := os.Rename(tmp.Name(), objectPath); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, errors.Wrap(err, "rename object")
	}

	// Persist the rename (per policy) so the object is durably visible.
	if err := s.syncDir(filepath.Dir(objectPath)); err != nil {
		return nil, err
	}

	if err := s.writeSidecar(req.Bucket, newSidecar(req.Key, etag, req.Metadata, req.Tags, req.ACL)); err != nil {
		return nil, err
	}

	return &fs.PutObjectResponse{ETag: etag}, nil
}

// currentObjectState reports whether the object at path exists and its ETag,
// preferring the sidecar's stored ETag and falling back to recompute-on-read.
func (s *Storage) currentObjectState(bucket, key, path string) (exists bool, etag string, err error) {
	info, statErr := os.Stat(path)
	if os.IsNotExist(statErr) {
		return false, "", nil
	}

	if statErr != nil {
		return false, "", errors.Wrap(statErr, "stat object")
	}

	etag, err = s.objectETag(bucket, key, path, info)
	if err != nil {
		return false, "", err
	}

	return true, etag, nil
}
