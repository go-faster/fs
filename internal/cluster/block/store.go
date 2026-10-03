// Package block stores object data as content-addressed blocks.
//
// A block is an immutable byte string named by its SHA-256. Its replicas are
// the first three slots of the layout partition its hash maps to, so any node
// can find a block from its hash alone, and identical content is stored once.
// Every read is verified: a block that rotted on disk is detected, dropped
// and fetched again from another replica, never served. Locally that is a
// CRC-32C stored with the block — hardware-accelerated, so a read is not
// bound by hashing — and a block from a peer is checked against its name.
//
// Blocks are referenced from object versions through the block_refs table;
// one with no live reference is collected after a grace period (see
// Manager.GC).
package block

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"
)

// format stamps a store's on-disk layout: each block file is the block
// followed by the big-endian CRC-32C of it. A store holding blocks without
// this stamp is refused rather than read as this format.
const format = "crc32c-trailer-1\n"

const formatFile = "FORMAT"

//nolint:gochecknoglobals // A CRC table is immutable and costly to rebuild.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Hash names a block: the SHA-256 of its content.
type Hash [sha256.Size]byte

// Sum returns the hash of data.
func Sum(data []byte) Hash { return sha256.Sum256(data) }

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// MarshalText encodes the hash as hex, in JSON and as a map key alike.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText decodes the hex form.
func (h *Hash) UnmarshalText(b []byte) error {
	v, err := ParseHash(string(b))
	if err != nil {
		return err
	}

	*h = v

	return nil
}

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
	// NoSync skips fsync: an acknowledged block can be lost in a crash. For
	// tests and development only.
	NoSync bool

	// stripes order a block's refresh by a writer against its removal by
	// GC: whichever comes second sees the first. Striped by the name's first
	// byte, so unrelated blocks rarely wait on each other.
	stripes [256]sync.Mutex
}

func (s *Store) stripe(name string) *sync.Mutex {
	b, _ := hex.DecodeString(name[:2])

	return &s.stripes[b[0]]
}

// NewStore returns the store rooted at dir, creating it.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, errors.Wrap(err, "create block dir")
	}

	stamp := filepath.Join(dir, formatFile)

	got, err := os.ReadFile(stamp) // #nosec G304 -- under the store root
	switch {
	case err == nil && string(got) == format:
	case err == nil:
		return nil, errors.Errorf("block store %s has format %q, this binary reads %q", dir, got, format)
	case errors.Is(err, fs.ErrNotExist):
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, errors.Wrap(err, "read block dir")
		}

		if len(entries) > 0 {
			return nil, errors.Errorf("block store %s holds blocks of an unknown format; rebuild it", dir)
		}

		if err := os.WriteFile(stamp, []byte(format), 0o600); err != nil {
			return nil, errors.Wrap(err, "stamp block store")
		}
	default:
		return nil, errors.Wrap(err, "read block store format")
	}

	return &Store{dir: dir}, nil
}

// path spreads blocks over two levels of directories by hash prefix, so no
// directory grows past a few thousand entries per million blocks.
func (s *Store) path(h Hash) string { return s.pathOf(h.String()) }

// pathOf is path for a file name that starts with a block's hash: the block
// itself, or one of its shards next to it.
func (s *Store) pathOf(name string) string {
	return filepath.Join(s.dir, name[:2], name[2:4], name)
}

// Put stores data under h; the caller vouches that h is its hash, having
// computed or checked it already. A block already present is not rewritten,
// but its time is refreshed: GC spares blocks touched within the grace
// period, which is what keeps a block being re-referenced by a new upload from
// being collected between its write and its reference.
func (s *Store) Put(h Hash, data []byte) error { return s.put(h.String(), data) }

func (s *Store) put(name string, data []byte) error {
	p := s.pathOf(name)
	mu := s.stripe(name)

	mu.Lock()
	now := time.Now()
	err := os.Chtimes(p, now, now)
	mu.Unlock()

	if err == nil {
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

	trailer := binary.BigEndian.AppendUint32(nil, crc32.Checksum(data, castagnoli))

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return errors.Wrap(err, "write block")
	}

	if _, err := f.Write(trailer); err != nil {
		_ = f.Close()

		return errors.Wrap(err, "write block")
	}

	if err := s.sync(f); err != nil {
		_ = f.Close()

		return errors.Wrap(err, "sync block")
	}

	if err := f.Close(); err != nil {
		return errors.Wrap(err, "close block")
	}

	mu.Lock()
	err = os.Rename(f.Name(), p)
	mu.Unlock()

	if err != nil {
		// Another writer placed the same block meanwhile — the same bytes,
		// since the name is their hash. Windows refuses a rename onto a file
		// another rename is placing, where POSIX replaces it.
		if _, statErr := os.Stat(p); statErr == nil {
			return nil
		}

		return errors.Wrap(err, "place block")
	}

	if s.NoSync {
		return nil
	}

	return syncDir(dir)
}

func (s *Store) sync(f *os.File) error {
	if s.NoSync {
		return nil
	}

	return f.Sync()
}

// Get returns the block's content, verified against its hash.
func (s *Store) Get(h Hash) ([]byte, error) { return s.get(h.String()) }

func (s *Store) get(name string) ([]byte, error) {
	p := s.pathOf(name)

	data, err := os.ReadFile(p) // #nosec G304 -- named by hash under the store root
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, errors.Wrap(err, "read block")
	}

	n := len(data) - crc32.Size
	if n < 0 || crc32.Checksum(data[:n], castagnoli) != binary.BigEndian.Uint32(data[n:]) {
		// A copy that fails its checksum is worse than none: drop it so the
		// replica reads as missing and is resynced.
		_ = os.Remove(p)

		return nil, ErrCorrupt
	}

	return data[:n:n], nil
}

// GetInto is Get reading into buf when it is large enough, so a caller that
// reads block after block can reuse one buffer instead of allocating each.
// The result aliases buf.
func (s *Store) GetInto(h Hash, buf []byte) ([]byte, error) {
	p := s.path(h)

	f, err := os.Open(p) // #nosec G304 -- named by hash under the store root
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, errors.Wrap(err, "open block")
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, errors.Wrap(err, "stat block")
	}

	size := int(info.Size())
	if cap(buf) < size {
		buf = make([]byte, size)
	}

	data := buf[:size]
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, errors.Wrap(err, "read block")
	}

	n := size - crc32.Size
	if n < 0 || crc32.Checksum(data[:n], castagnoli) != binary.BigEndian.Uint32(data[n:]) {
		_ = os.Remove(p)

		return nil, ErrCorrupt
	}

	// The trailer stays in the buffer's capacity, so the whole buffer comes
	// back when the caller reuses it.
	return data[:n], nil
}

// deleteIfOlder removes the named file if it was last written or touched
// before cutoff, and reports whether it did. GC decides a block is dead
// from a reference check that a concurrent writer can overtake; the writer
// touches the block first, so a fresh time here means it is wanted.
func (s *Store) deleteIfOlder(name string, cutoff time.Time) (bool, error) {
	mu := s.stripe(name)
	mu.Lock()
	defer mu.Unlock()

	p := s.pathOf(name)

	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, errors.Wrap(err, "stat block")
	}

	if info.ModTime().After(cutoff) {
		return false, nil
	}

	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, errors.Wrap(err, "delete block")
	}

	return true, nil
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

// Shard names one shard of an erasure-coded block: index I of the K+M shards
// of the block named Hash.
type Shard struct {
	Hash Hash
	K, M int
	I    int
}

func (sh Shard) String() string { return fmt.Sprintf("%s.%d-%d-%d", sh.Hash, sh.K, sh.M, sh.I) }

// ParseShard parses a shard's name.
func ParseShard(name string) (Shard, error) {
	hash, rest, ok := strings.Cut(name, ".")

	h, err := ParseHash(hash)
	if !ok || err != nil {
		return Shard{}, errors.Errorf("bad shard name %q", name)
	}

	sh := Shard{Hash: h}
	if _, err := fmt.Sscanf(rest, "%d-%d-%d", &sh.K, &sh.M, &sh.I); err != nil ||
		sh.K < 1 || sh.M < 1 || sh.I < 0 || sh.I >= sh.K+sh.M || sh.String() != name {
		return Shard{}, errors.Errorf("bad shard name %q", name)
	}

	return sh, nil
}

// PutShard stores a shard, as Put does a block. Its integrity at rest is the
// CRC-32C every file carries; it has no hash of its own to be checked by.
func (s *Store) PutShard(sh Shard, data []byte) error { return s.put(sh.String(), data) }

// GetShard returns a shard, verified against its CRC.
func (s *Store) GetShard(sh Shard) ([]byte, error) { return s.get(sh.String()) }

// HasShard reports whether the shard is present, without verifying it.
func (s *Store) HasShard(sh Shard) bool {
	_, err := os.Stat(s.pathOf(sh.String()))

	return err == nil
}

// DeleteShard removes a shard; removing one that is absent is not an error.
func (s *Store) DeleteShard(sh Shard) error {
	if err := os.Remove(s.pathOf(sh.String())); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Wrap(err, "delete shard")
	}

	return nil
}

// WalkShards calls fn for every shard with its last write or touch time.
// Walk reports whole blocks and temporary files; this, only shards.
func (s *Store) WalkShards(fn func(sh Shard, mod time.Time) error) error {
	return filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.Contains(d.Name(), ".") || strings.HasSuffix(d.Name(), tmpSuffix) {
			return err
		}

		sh, err := ParseShard(d.Name())
		if err != nil {
			return nil //nolint:nilerr // Not a shard; not ours to report.
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		return fn(sh, info.ModTime())
	})
}
