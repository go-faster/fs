package storagefs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// metaDir is the root-level directory holding metadata sidecars, laid out as
// .meta/<bucket>/<sha256(key)>.json. It lives outside the bucket directories
// so sidecars can never collide with (or show up as) object keys.
const metaDir = ".meta"

// sidecarVersion stamps the on-disk sidecar format; bump on incompatible
// changes and keep readers tolerant of older versions.
const sidecarVersion = 1

// sidecar is the persistent per-object metadata document. A missing sidecar is
// always valid: pre-sidecar data directories stay readable with defaults, and
// the ETag falls back to recompute-on-read.
type sidecar struct {
	Version int `json:"version"`
	// Key is stored for debuggability only; the file name is a hash.
	Key                string            `json:"key"`
	ETag               string            `json:"etag,omitempty"`
	ContentType        string            `json:"content_type,omitempty"`
	CacheControl       string            `json:"cache_control,omitempty"`
	ContentDisposition string            `json:"content_disposition,omitempty"`
	ContentEncoding    string            `json:"content_encoding,omitempty"`
	UserMetadata       map[string]string `json:"user_metadata,omitempty"`
	Tags               []fs.Tag          `json:"tags,omitempty"`
	ACL                fs.ACL            `json:"acl,omitempty"`
}

// metadata converts the sidecar's header fields to the domain type.
func (sc *sidecar) metadata() fs.ObjectMetadata {
	return fs.ObjectMetadata{
		ContentType:        sc.ContentType,
		CacheControl:       sc.CacheControl,
		ContentDisposition: sc.ContentDisposition,
		ContentEncoding:    sc.ContentEncoding,
		UserMetadata:       sc.UserMetadata,
	}
}

// newSidecar builds a sidecar document for an object.
func newSidecar(key, etag string, meta fs.ObjectMetadata, tags []fs.Tag, acl fs.ACL) *sidecar {
	return &sidecar{
		Version:            sidecarVersion,
		Key:                key,
		ETag:               etag,
		ContentType:        meta.ContentType,
		CacheControl:       meta.CacheControl,
		ContentDisposition: meta.ContentDisposition,
		ContentEncoding:    meta.ContentEncoding,
		UserMetadata:       meta.UserMetadata,
		Tags:               tags,
		ACL:                acl,
	}
}

// sidecarPath returns the sidecar location for an object. The key is hashed so
// the flat layout is immune to key length, separators, and name collisions.
func (s *Storage) sidecarPath(bucket, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.root, metaDir, bucket, hex.EncodeToString(sum[:])+".json")
}

// readSidecar loads an object's sidecar. A missing sidecar returns (nil, nil).
func (s *Storage) readSidecar(bucket, key string) (*sidecar, error) {
	data, err := os.ReadFile(s.sidecarPath(bucket, key)) //nolint:gosec // Path is derived from a hash of the key.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, errors.Wrap(err, "read sidecar")
	}

	var sc sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		// A corrupt sidecar must not make the object unreadable; treat it as
		// absent (defaults + ETag recompute).
		return nil, nil //nolint:nilerr // Deliberate: degrade to sidecar-less behavior.
	}

	return &sc, nil
}

// writeSidecar persists an object's sidecar atomically (temp file + rename),
// with fsync per the storage's sync policy.
func (s *Storage) writeSidecar(bucket string, sc *sidecar) error {
	path := s.sidecarPath(bucket, sc.Key)

	if err := os.MkdirAll(filepath.Dir(path), defaultDirPermissions); err != nil {
		return errors.Wrap(err, "create sidecar directory")
	}

	data, err := json.Marshal(sc)
	if err != nil {
		return errors.Wrap(err, "marshal sidecar")
	}

	return s.atomicWrite(path, data)
}

// deleteSidecar removes an object's sidecar; a missing sidecar is fine.
func (s *Storage) deleteSidecar(bucket, key string) {
	_ = os.Remove(s.sidecarPath(bucket, key))
}

// deleteBucketMeta removes a bucket's whole sidecar directory.
func (s *Storage) deleteBucketMeta(bucket string) {
	_ = os.RemoveAll(filepath.Join(s.root, metaDir, bucket))
}

// objectETag resolves an object's ETag, preferring the sidecar's stored value
// and falling back to (cached) recompute-on-read for sidecar-less files.
func (s *Storage) objectETag(bucket, key, path string, info os.FileInfo) (string, error) {
	if sc, err := s.readSidecar(bucket, key); err == nil && sc != nil && sc.ETag != "" {
		return sc.ETag, nil
	}

	return s.etagFor(path, info)
}
