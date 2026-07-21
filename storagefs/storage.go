// Package storagefs implements fs.Storage.
package storagefs

import (
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

var _ fs.Storage = (*Storage)(nil)

// stagingSubdir holds in-progress object bodies before they are renamed into
// place. Keeping it outside the bucket tree means a crash mid-write never leaves
// a partial file where ListObjects could see it. It is a root-level dot-dir, so
// it is excluded from bucket listings.
const stagingSubdir = ".tmp"

func New(root string, opts ...Option) (*Storage, error) {
	if err := os.MkdirAll(root, 0750); err != nil {
		return nil, fmt.Errorf("failed to create root directory: %w", err)
	}

	s := &Storage{
		root:      root,
		multipart: newMultipartManager(root),
	}
	for _, opt := range opts {
		opt(s)
	}

	if err := os.MkdirAll(s.stagingDir(), defaultDirPermissions); err != nil {
		return nil, fmt.Errorf("failed to create staging directory: %w", err)
	}

	return s, nil
}

type Storage struct {
	root      string
	multipart *multipartManager

	// sync is the durability policy applied to writes.
	sync SyncPolicy

	etagMu    sync.Mutex
	etagCache map[string]etagEntry

	// metaMu serializes sidecar read-modify-write cycles (tagging updates).
	metaMu sync.Mutex

	// putMu serializes the finalize step of PutObject (conditional-write
	// evaluation, rename into place, and sidecar write) so concurrent
	// conditional PUTs to the same key resolve to a single winner. The body is
	// streamed to a temp file outside this lock, so only the fast rename step
	// is serialized.
	putMu sync.Mutex
}

// etagEntry is a cached ETag valid as long as size and modtime are unchanged.
type etagEntry struct {
	size    int64
	modNano int64
	etag    string
}

// etagFor returns the hex MD5 ETag of the file at path, computing it lazily and
// caching the result keyed by (path, size, modtime). The ETag is recomputed when
// the file changes.
func (s *Storage) etagFor(path string, info os.FileInfo) (string, error) {
	size, modNano := info.Size(), info.ModTime().UnixNano()

	s.etagMu.Lock()
	if e, ok := s.etagCache[path]; ok && e.size == size && e.modNano == modNano {
		s.etagMu.Unlock()
		return e.etag, nil
	}
	s.etagMu.Unlock()

	// #nosec G304 -- path is constructed from validated bucket and key.
	f, err := os.Open(path)
	if err != nil {
		return "", errors.Wrap(err, "open object")
	}
	defer func() { _ = f.Close() }()

	h := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.
	if _, err := io.Copy(h, f); err != nil {
		return "", errors.Wrap(err, "hash object")
	}

	etag := hex.EncodeToString(h.Sum(nil))

	s.etagMu.Lock()
	if s.etagCache == nil {
		s.etagCache = make(map[string]etagEntry)
	}

	s.etagCache[path] = etagEntry{size: size, modNano: modNano, etag: etag}
	s.etagMu.Unlock()

	return etag, nil
}

// stagingDir returns the root-level staging directory for in-progress writes.
func (s *Storage) stagingDir() string {
	return filepath.Join(s.root, stagingSubdir)
}

// newObjectTemp creates a temp file in the staging directory for an object body
// that will be renamed into its bucket. Staging and bucket dirs share the root
// filesystem, so the rename is atomic.
func (s *Storage) newObjectTemp() (*os.File, error) {
	f, err := os.CreateTemp(s.stagingDir(), "obj-*")
	if err != nil {
		return nil, errors.Wrap(err, "create temp object")
	}

	return f, nil
}
