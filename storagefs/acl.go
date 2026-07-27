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
	// Owner is the principal that created the bucket; absent for buckets
	// created before ownership was recorded.
	Owner fs.Owner `json:"owner,omitzero"`
	// CORS is the bucket's rule set, as set through the ?cors subresource.
	CORS []fs.CORSRule `json:"cors,omitempty"`
	// PublicAccessBlock is the ?publicAccessBlock configuration; nil when the
	// bucket has none.
	PublicAccessBlock *fs.PublicAccessBlock `json:"public_access_block,omitempty"`
	// ObjectOwnership is the ?ownershipControls rule; empty when unset.
	ObjectOwnership string `json:"object_ownership,omitempty"`
	// Encryption is the ?encryption default algorithm; empty when unset.
	Encryption string `json:"encryption,omitempty"`
	// Versioning is the bucket's versioning state; empty means never
	// configured, which is distinct from Suspended.
	Versioning fs.VersioningState `json:"versioning,omitempty"`
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

	return s.atomicWrite(path, data)
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

// SetObjectACL rewrites the object's sidecar with the new canned ACL, creating
// a sidecar for pre-sidecar objects. The object content is not touched.
func (s *Storage) SetObjectACL(_ context.Context, bucket, key string, acl fs.ACL) error {
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
		sc = newSidecar(key, "", "", fs.ObjectMetadata{}, nil, acl, fs.Owner{})
	}

	sc.ACL = acl

	return s.writeSidecar(bucket, sc)
}

// ObjectOwner returns the principal recorded when the object was written.
// Objects stored before owners were modeled report the zero owner.
func (s *Storage) ObjectOwner(_ context.Context, bucket, key string) (fs.Owner, error) {
	if err := s.statObject(bucket, key); err != nil {
		return fs.Owner{}, err
	}

	sc, err := s.readSidecar(bucket, key)
	if err != nil || sc == nil {
		return fs.Owner{}, err
	}

	return sc.owner(), nil
}

// normalizeACL defaults an unset (zero-value) ACL to ACLPrivate.
func normalizeACL(a fs.ACL) fs.ACL {
	if a == "" {
		return fs.ACLPrivate
	}

	return a
}

// BucketCORS implements fs.BucketCORSStore.
func (s *Storage) BucketCORS(_ context.Context, bucket string) ([]fs.CORSRule, error) {
	if !s.bucketExists(bucket) {
		return nil, fs.ErrBucketNotFound
	}

	s.metaMu.RLock()
	defer s.metaMu.RUnlock()

	return s.readBucketMeta(bucket).CORS, nil
}

// SetBucketCORS implements fs.BucketCORSStore.
func (s *Storage) SetBucketCORS(_ context.Context, bucket string, rules []fs.CORSRule) error {
	if !s.bucketExists(bucket) {
		return fs.ErrBucketNotFound
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	meta := s.readBucketMeta(bucket)
	meta.CORS = rules

	return s.writeBucketMeta(bucket, meta)
}

// DeleteBucketCORS implements fs.BucketCORSStore.
func (s *Storage) DeleteBucketCORS(ctx context.Context, bucket string) error {
	return s.SetBucketCORS(ctx, bucket, nil)
}

// BucketPublicAccessBlock implements fs.BucketSettingsStore.
func (s *Storage) BucketPublicAccessBlock(_ context.Context, bucket string) (*fs.PublicAccessBlock, error) {
	if !s.bucketExists(bucket) {
		return nil, fs.ErrBucketNotFound
	}

	s.metaMu.RLock()
	defer s.metaMu.RUnlock()

	return s.readBucketMeta(bucket).PublicAccessBlock, nil
}

// SetBucketPublicAccessBlock implements fs.BucketSettingsStore; nil clears it.
func (s *Storage) SetBucketPublicAccessBlock(_ context.Context, bucket string, block *fs.PublicAccessBlock) error {
	if !s.bucketExists(bucket) {
		return fs.ErrBucketNotFound
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	meta := s.readBucketMeta(bucket)
	meta.PublicAccessBlock = block

	return s.writeBucketMeta(bucket, meta)
}

// BucketObjectOwnership implements fs.BucketSettingsStore.
func (s *Storage) BucketObjectOwnership(_ context.Context, bucket string) (string, error) {
	if !s.bucketExists(bucket) {
		return "", fs.ErrBucketNotFound
	}

	s.metaMu.RLock()
	defer s.metaMu.RUnlock()

	return s.readBucketMeta(bucket).ObjectOwnership, nil
}

// SetBucketObjectOwnership implements fs.BucketSettingsStore; empty clears it.
func (s *Storage) SetBucketObjectOwnership(_ context.Context, bucket, ownership string) error {
	if !s.bucketExists(bucket) {
		return fs.ErrBucketNotFound
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	meta := s.readBucketMeta(bucket)
	meta.ObjectOwnership = ownership

	return s.writeBucketMeta(bucket, meta)
}

// BucketEncryption implements fs.BucketEncrypter.
func (s *Storage) BucketEncryption(_ context.Context, bucket string) (string, error) {
	if !s.bucketExists(bucket) {
		return "", fs.ErrBucketNotFound
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	return s.readBucketMeta(bucket).Encryption, nil
}

// SetBucketEncryption implements fs.BucketEncrypter.
func (s *Storage) SetBucketEncryption(_ context.Context, bucket, algorithm string) error {
	if !s.bucketExists(bucket) {
		return fs.ErrBucketNotFound
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	meta := s.readBucketMeta(bucket)
	meta.Encryption = algorithm

	return s.writeBucketMeta(bucket, meta)
}

// SetBucketVersioning implements fs.Versioner.
func (s *Storage) SetBucketVersioning(_ context.Context, bucket string, state fs.VersioningState) error {
	if !s.bucketExists(bucket) {
		return fs.ErrBucketNotFound
	}

	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	meta := s.readBucketMeta(bucket)
	meta.Versioning = state

	return s.writeBucketMeta(bucket, meta)
}

// BucketVersioning implements fs.Versioner.
func (s *Storage) BucketVersioning(_ context.Context, bucket string) (fs.VersioningState, error) {
	if !s.bucketExists(bucket) {
		return fs.VersioningUnset, fs.ErrBucketNotFound
	}

	s.metaMu.RLock()
	defer s.metaMu.RUnlock()

	return s.readBucketMeta(bucket).Versioning, nil
}
