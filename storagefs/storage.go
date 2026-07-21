// Package storagefs implements fs.Storage.
package storagefs

import (
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

var _ fs.Storage = (*Storage)(nil)

func New(root string) (*Storage, error) {
	if err := os.MkdirAll(root, 0750); err != nil {
		return nil, fmt.Errorf("failed to create root directory: %w", err)
	}

	return &Storage{
		root:      root,
		multipart: newMultipartManager(root),
	}, nil
}

type Storage struct {
	root      string
	multipart *multipartManager

	etagMu    sync.Mutex
	etagCache map[string]etagEntry

	// metaMu serializes sidecar read-modify-write cycles (tagging updates).
	metaMu sync.Mutex
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
