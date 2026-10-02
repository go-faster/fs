// Package block stores object data as content-addressed blocks.
//
// A block is an immutable byte string named by its SHA-256. Its replicas are
// the first three slots of the layout partition its hash maps to, so any node
// can find a block from its hash alone, and identical content is stored once.
// Every read verifies the hash: a block that rotted on disk is detected,
// dropped and fetched again from another replica, never served.
//
// Blocks are referenced from object versions through the block_refs table;
// one with no live reference is collected after a grace period (see
// Manager.GC).
package block

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/go-faster/errors"
)

// Hash names a block: the SHA-256 of its content.
type Hash [sha256.Size]byte

// Sum returns the hash of data.
func Sum(data []byte) Hash { return sha256.Sum256(data) }

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// ParseHash parses a hash's hex form.
func ParseHash(s string) (Hash, error) {
	var h Hash

	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) {
		return h, errors.Errorf("bad block hash %q", s)
	}

	copy(h[:], b)

	return h, nil
}

var (
	// ErrNotFound is returned for a block this node does not hold.
	ErrNotFound = errors.New("block not found")
	// ErrCorrupt is returned for a block whose content no longer matches its
	// hash. The bad copy is removed before the error is returned.
	ErrCorrupt = errors.New("block corrupt")
	// ErrMismatch is returned when data offered for a hash does not hash to
	// it.
	ErrMismatch = errors.New("block content does not match its hash")
)

// tmpSuffix marks a block still being written. Collected after the grace
// period, as a crash mid-write leaves one behind.
const tmpSuffix = ".tmp"

// Store is the blocks one node holds, on its local disk.
type Store struct {
	dir string
}

// NewStore returns the store rooted at dir, creating it.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, errors.Wrap(err, "create block dir")
	}

	return &Store{dir: dir}, nil
}

// path spreads blocks over two levels of directories by hash prefix, so no
// directory grows past a few thousand entries per million blocks.
func (s *Store) path(h Hash) string {
	name := h.String()

	return filepath.Join(s.dir, name[:2], name[2:4], name)
}

// Put stores data under h. A block already present is not rewritten, but its
// time is refreshed: GC spares blocks touched within the grace period, which
// is what keeps a block being re-referenced by a new upload from being
// collected between its write and its reference.
func (s *Store) Put(h Hash, data []byte) error {
	if Sum(data) != h {
		return ErrMismatch
	}

	p := s.path(h)

	now := time.Now()
	if err := os.Chtimes(p, now, now); err == nil {
		return nil
	}

	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return errors.Wrap(err, "create block dir")
	}

	f, err := os.CreateTemp(dir, filepath.Base(p)+".*"+tmpSuffix)
	if err != nil {
		return errors.Wrap(err, "create block")
	}

	defer func() { _ = os.Remove(f.Name()) }()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return errors.Wrap(err, "write block")
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return errors.Wrap(err, "sync block")
	}

	if err := f.Close(); err != nil {
		return errors.Wrap(err, "close block")
	}

	if err := os.Rename(f.Name(), p); err != nil {
		return errors.Wrap(err, "place block")
	}

	return syncDir(dir)
}

// Get returns the block's content, verified against its hash.
func (s *Store) Get(h Hash) ([]byte, error) {
	p := s.path(h)

	data, err := os.ReadFile(p) // #nosec G304 -- named by hash under the store root
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, errors.Wrap(err, "read block")
	}

	if Sum(data) != h {
		// A copy that fails its hash is worse than none: drop it so the
		// replica reads as missing and is resynced.
		_ = os.Remove(p)

		return nil, ErrCorrupt
	}

	return data, nil
}

// Has reports whether the block is present, without verifying it.
func (s *Store) Has(h Hash) bool {
	_, err := os.Stat(s.path(h))

	return err == nil
}

// Delete removes the block; removing one that is absent is not an error.
func (s *Store) Delete(h Hash) error {
	if err := os.Remove(s.path(h)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Wrap(err, "delete block")
	}

	return nil
}

// Walk calls fn for every block with its last write or touch time. Leftover
// temporary files are reported with tmp set and a zero hash.
func (s *Store) Walk(fn func(h Hash, tmp bool, mod time.Time, path string) error) error {
	return filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // Collected or replaced while walking.
			}

			return err
		}

		if strings.HasSuffix(d.Name(), tmpSuffix) {
			return fn(Hash{}, true, info.ModTime(), p)
		}

		h, err := ParseHash(d.Name())
		if err != nil {
			return nil // Not ours; leave it alone.
		}

		return fn(h, false, info.ModTime(), p)
	})
}

// syncDir makes a rename into dir durable.
func syncDir(dir string) error {
	// Windows journals directory metadata and cannot sync a directory handle.
	if runtime.GOOS == "windows" {
		return nil
	}

	d, err := os.Open(dir) // #nosec G304 -- a directory under the store root
	if err != nil {
		return errors.Wrap(err, "open block dir")
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return errors.Wrap(err, "sync block dir")
	}

	return nil
}
