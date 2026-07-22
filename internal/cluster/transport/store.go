package transport

import (
	"bytes"
	"context"
	"io"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// Store is the node-local fragment storage the transport serves. Names are
// slash-separated, path-clean identifiers chosen by the coordinator (they never
// escape the disk namespace). Create must be atomic: a fragment becomes visible
// only when the returned writer is closed successfully.
type Store interface {
	// Create opens a writer for a new fragment (replacing any existing one on
	// successful close).
	Create(ctx context.Context, disk cluster.DiskID, name string) (io.WriteCloser, error)
	// Open returns the fragment payload and its size, or ErrNotFound.
	Open(ctx context.Context, disk cluster.DiskID, name string) (io.ReadCloser, int64, error)
	// Stat reports the fragment's size, or ErrNotFound.
	Stat(ctx context.Context, disk cluster.DiskID, name string) (int64, error)
	// Delete removes the fragment, or ErrNotFound.
	Delete(ctx context.Context, disk cluster.DiskID, name string) error
	// List returns the names on a disk with the given prefix, sorted
	// lexicographically. An unknown prefix is an empty listing, not an error.
	List(ctx context.Context, disk cluster.DiskID, prefix string) ([]string, error)
}

// ValidName reports whether a fragment name is safe to serve: non-empty,
// path-clean, relative and free of dot-dot traversal.
func ValidName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") {
		return false
	}

	clean := path.Clean(name)
	if clean != name {
		return false
	}

	if clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}

	return clean != "."
}

// MemStore is an in-memory Store for tests and in-process clusters.
type MemStore struct {
	mu    sync.RWMutex
	frags map[memKey][]byte
}

type memKey struct {
	disk cluster.DiskID
	name string
}

// NewMemStore builds an empty in-memory fragment store.
func NewMemStore() *MemStore {
	return &MemStore{frags: make(map[memKey][]byte)}
}

// memWriter buffers a fragment and commits it on Close (atomic visibility).
type memWriter struct {
	s   *MemStore
	key memKey
	buf bytes.Buffer
}

func (w *memWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func (w *memWriter) Close() error {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()

	w.s.frags[w.key] = append([]byte(nil), w.buf.Bytes()...)

	return nil
}

// Create implements Store.
func (s *MemStore) Create(_ context.Context, disk cluster.DiskID, name string) (io.WriteCloser, error) {
	if !ValidName(name) {
		return nil, errors.Errorf("invalid fragment name %q", name)
	}

	return &memWriter{s: s, key: memKey{disk: disk, name: name}}, nil
}

// Open implements Store.
func (s *MemStore) Open(_ context.Context, disk cluster.DiskID, name string) (io.ReadCloser, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, ok := s.frags[memKey{disk: disk, name: name}]
	if !ok {
		return nil, 0, ErrNotFound
	}

	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// Stat implements Store.
func (s *MemStore) Stat(_ context.Context, disk cluster.DiskID, name string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, ok := s.frags[memKey{disk: disk, name: name}]
	if !ok {
		return 0, ErrNotFound
	}

	return int64(len(data)), nil
}

// List implements Store.
func (s *MemStore) List(_ context.Context, disk cluster.DiskID, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var names []string

	for key := range s.frags {
		if key.disk == disk && strings.HasPrefix(key.name, prefix) {
			names = append(names, key.name)
		}
	}

	sort.Strings(names)

	return names, nil
}

// Delete implements Store.
func (s *MemStore) Delete(_ context.Context, disk cluster.DiskID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := memKey{disk: disk, name: name}
	if _, ok := s.frags[key]; !ok {
		return ErrNotFound
	}

	delete(s.frags, key)

	return nil
}
