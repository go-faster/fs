package storagefs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// bucketMetaFile is the bucket-level metadata sidecar under
// .meta/<bucket>/bucket.json. It cannot collide with object sidecars, which are
// named by the 64-hex SHA-256 of the key.
const bucketMetaFile = "bucket.json"

// bucketMeta is the persistent bucket-level metadata document.
type bucketMeta struct {
	Version int    `json:"version"`
	ACL     fs.ACL `json:"acl,omitempty"`
}

func (s *Storage) bucketMetaPath(bucket string) string {
	return filepath.Join(s.root, metaDir, bucket, bucketMetaFile)
}

// readBucketMeta loads a bucket's metadata; a missing or corrupt document
// returns defaults.
func (s *Storage) readBucketMeta(bucket string) bucketMeta {
	data, err := os.ReadFile(s.bucketMetaPath(bucket)) //nolint:gosec // Path built from a validated bucket name.
	if err != nil {
		return bucketMeta{Version: sidecarVersion}
	}

	var m bucketMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return bucketMeta{Version: sidecarVersion}
	}

	return m
}

func (s *Storage) writeBucketMeta(bucket string, m bucketMeta) error {
	path := s.bucketMetaPath(bucket)

	if err := os.MkdirAll(filepath.Dir(path), defaultDirPermissions); err != nil {
		return errors.Wrap(err, "create bucket meta directory")
	}

	m.Version = sidecarVersion

	data, err := json.Marshal(m)
	if err != nil {
		return errors.Wrap(err, "marshal bucket meta")
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return errors.Wrap(err, "create bucket meta temp file")
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())

		return errors.Wrap(err, "write bucket meta")
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return errors.Wrap(err, "close bucket meta")
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return errors.Wrap(err, "rename bucket meta")
	}

	return nil
}

func (s *Storage) bucketExists(bucket string) bool {
	_, err := os.Stat(filepath.Join(s.root, bucket))

	return err == nil
}

func (s *Storage) SetBucketACL(_ context.Context, bucket string, acl fs.ACL) error {
	if !s.bucketExists(bucket) {
		return fs.ErrBucketNotFound
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	m := s.readBucketMeta(bucket)
	m.ACL = acl

	return s.writeBucketMeta(bucket, m)
}

func (s *Storage) BucketACL(_ context.Context, bucket string) (fs.ACL, error) {
	if !s.bucketExists(bucket) {
		return fs.ACLPrivate, fs.ErrBucketNotFound
	}

	return normalizeACL(s.readBucketMeta(bucket).ACL), nil
}

func (s *Storage) ObjectACL(_ context.Context, bucket, key string) (fs.ACL, error) {
	if err := s.statObject(bucket, key); err != nil {
		return fs.ACLPrivate, err
	}

	sc, err := s.readSidecar(bucket, key)
	if err != nil || sc == nil {
		return fs.ACLPrivate, err
	}

	return normalizeACL(sc.ACL), nil
}

// normalizeACL defaults an unset (zero-value) ACL to ACLPrivate.
func normalizeACL(a fs.ACL) fs.ACL {
	if a == "" {
		return fs.ACLPrivate
	}

	return a
}
