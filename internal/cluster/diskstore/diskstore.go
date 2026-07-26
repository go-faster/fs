// Package diskstore is the filesystem-backed fragment store for go-faster/fs
// cluster mode: the durable transport.Store a production node serves its
// fragments from, one root directory per disk. It follows the storagefs
// durability protocol — every fragment is written to a temp file and renamed
// into place, so a fragment is visible only once complete (never torn), and
// the configured storagefs.SyncPolicy decides whether an acknowledged write
// also survives power loss.
//
// Fragment names are the slash-separated, transport.ValidName-checked
// identifiers the clusterstore coordinator mints (hash-based directories,
// generation-stamped files); they map directly onto a relative path under the
// disk root. Stale ".tmp-*" files left by a crash are invisible to reads and
// are swept by the scrubber (Phase 8).
package diskstore

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/transport"
	"github.com/go-faster/fs/storagefs"
)

// dirPermissions is the mode for created fragment directories.
const dirPermissions = 0o750

// tmpPattern names in-flight temp files; they never collide with fragment
// names (ValidName segments are never empty, and fragments are renamed into
// place with their final name).
const tmpPattern = ".tmp-*"

// Store is a transport.Store over one filesystem root per disk. It is safe
// for concurrent use; concurrent writes to the same name last-close-wins,
// matching MemStore.
type Store struct {
	roots map[cluster.DiskID]string
	sync  storagefs.SyncPolicy
	// index tracks per-disk occupancy so a drain can be watched without
	// walking the tree for every poll. One entry per root, created in New.
	index map[cluster.DiskID]*diskIndex
}

var _ transport.Store = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithSyncPolicy sets the durability policy (default storagefs.SyncNone; the
// binary passes storagefs.SyncFileDir).
func WithSyncPolicy(p storagefs.SyncPolicy) Option {
	return func(s *Store) { s.sync = p }
}

// New builds a Store serving the given disk roots, creating each root
// directory if needed.
func New(roots map[cluster.DiskID]string, opts ...Option) (*Store, error) {
	if len(roots) == 0 {
		return nil, errors.New("diskstore: no disk roots")
	}

	s := &Store{
		roots: make(map[cluster.DiskID]string, len(roots)),
		index: make(map[cluster.DiskID]*diskIndex, len(roots)),
	}

	for disk, root := range roots {
		if disk == "" {
			return nil, errors.New("diskstore: empty disk ID")
		}

		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, errors.Wrapf(err, "resolve root for disk %q", disk)
		}

		if err := os.MkdirAll(abs, dirPermissions); err != nil {
			return nil, errors.Wrapf(err, "create root for disk %q", disk)
		}

		s.roots[disk] = abs
		s.index[disk] = newDiskIndex(abs)
	}

	for _, o := range opts {
		o(s)
	}

	// Invalidate every adopted checkpoint before serving a single write: from
	// here on the file on disk says "rescan me", so only an orderly Close can
	// hand the counters to the next start.
	if err := s.Checkpoint(false); err != nil {
		return nil, err
	}

	return s, nil
}

// Close checkpoints the index so the next start adopts the counters instead of
// walking every disk. A store that is not closed is not damaged by it — the
// counters are simply rebuilt by a scan.
func (s *Store) Close() error {
	return s.Checkpoint(true)
}

// path resolves a fragment name under its disk root, rejecting unknown disks
// and unsafe names.
func (s *Store) path(disk cluster.DiskID, name string) (string, error) {
	root, ok := s.roots[disk]
	if !ok {
		return "", errors.Errorf("unknown disk %q", disk)
	}

	if !transport.ValidName(name) {
		return "", errors.Errorf("invalid fragment name %q", name)
	}

	return filepath.Join(root, filepath.FromSlash(name)), nil
}

// Create implements transport.Store: the fragment is staged in a temp file
// next to its final location and renamed into place on Close, so it becomes
// visible atomically (and durably, per the sync policy).
func (s *Store) Create(_ context.Context, disk cluster.DiskID, name string) (io.WriteCloser, error) {
	path, err := s.path(disk, name)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPermissions); err != nil {
		return nil, errors.Wrap(err, "create fragment directory")
	}

	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return nil, errors.Wrap(err, "create temp file")
	}

	return &fileWriter{store: s, disk: disk, tmp: tmp, path: path}, nil
}

// Open implements transport.Store.
func (s *Store) Open(_ context.Context, disk cluster.DiskID, name string) (io.ReadCloser, int64, error) {
	path, err := s.path(disk, name)
	if err != nil {
		return nil, 0, err
	}

	f, err := os.Open(path) //nolint:gosec // Path is root-joined from a ValidName-checked name.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, transport.ErrNotFound
		}

		return nil, 0, errors.Wrap(err, "open fragment")
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, errors.Wrap(err, "stat fragment")
	}

	return f, info.Size(), nil
}

// Stat implements transport.Store.
func (s *Store) Stat(_ context.Context, disk cluster.DiskID, name string) (int64, error) {
	path, err := s.path(disk, name)
	if err != nil {
		return 0, err
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, transport.ErrNotFound
		}

		return 0, errors.Wrap(err, "stat fragment")
	}

	if info.IsDir() {
		// A directory is fragment namespace structure, not a fragment.
		return 0, transport.ErrNotFound
	}

	return info.Size(), nil
}

// Delete implements transport.Store, pruning fragment directories left empty
// so hash-based namespaces do not accumulate forever.
func (s *Store) Delete(_ context.Context, disk cluster.DiskID, name string) error {
	path, err := s.path(disk, name)
	if err != nil {
		return err
	}

	// The size is read before the unlink because afterwards nobody can: the
	// index has to be told what left, not just that something did.
	var size int64

	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return transport.ErrNotFound
		}

		size = info.Size()
	}

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return transport.ErrNotFound
		}

		return errors.Wrap(err, "delete fragment")
	}

	s.index[disk].removed(size)
	s.pruneEmptyDirs(filepath.Dir(path), s.roots[disk])

	return nil
}

// skipEntry reports whether a directory entry is store bookkeeping rather than
// a fragment: in-flight temp files and the index checkpoint at the disk root.
//
// Every dot-prefixed name is store-private. Fragment paths are minted by the
// coordinator out of "obj", hex hashes, hex generations and "meta", so no
// segment of one ever begins with a dot — which is what makes the dot a safe
// namespace to reserve.
func skipEntry(name string) bool { return strings.HasPrefix(name, ".") }

// pruneEmptyDirs removes now-empty parents of a deleted fragment, stopping at
// the disk root or the first non-empty directory. Best-effort: a concurrent
// create racing the prune simply keeps the directory.
func (*Store) pruneEmptyDirs(dir, root string) {
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			return
		}

		dir = filepath.Dir(dir)
	}
}

// dirBatch is how many directory entries a drain probe reads at a time. It
// exists so HasData never materializes a directory: one bucket directory holds
// one entry per object, which on a full disk is millions.
const dirBatch = 32

// HasData reports whether the disk holds any fragment at all.
//
// This is the drain question — "has this disk been emptied yet?" — and it is
// deliberately a boolean rather than an object count. An orchestrator
// decommissioning a node needs to know when its data is gone and the volume
// can be deleted, and capacity cannot tell it: the bytes come from statfs, so
// they include filesystem overhead and a disk holding nothing never reports
// zero used. A count cannot be produced cheaply — there is no index, so
// counting means walking the tree — while the boolean is answerable exactly,
// and in constant time on a drained disk, which is the case that must be fast.
//
// Cost: it descends to the first fragment and stops, reading directories in
// batches, so it never lists a large one. A drained disk costs a single failed
// open of the root (Delete prunes emptied directories, so the tree is gone);
// a loaded disk costs one batch per level down to the first fragment.
// In-flight temp files do not count: they are not fragments yet, and a crash
// leaves them behind.
func (s *Store) HasData(_ context.Context, disk cluster.DiskID) (bool, error) {
	root, ok := s.roots[disk]
	if !ok {
		return false, errors.Errorf("unknown disk %q", disk)
	}

	found, err := hasFragment(root)
	if err != nil {
		return false, errors.Wrap(err, "probe disk")
	}

	return found, nil
}

// hasFragment reports whether dir holds a fragment at any depth, stopping at
// the first one found.
func hasFragment(dir string) (bool, error) {
	f, err := os.Open(dir) //nolint:gosec // Path is the disk root, or a directory found beneath it.
	if err != nil {
		if os.IsNotExist(err) {
			// Pruned by a concurrent Delete, or never created.
			return false, nil
		}

		return false, errors.Wrap(err, "open directory")
	}

	defer func() {
		_ = f.Close()
	}()

	for {
		entries, err := f.ReadDir(dirBatch)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}

			return false, errors.Wrap(err, "read directory")
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				if skipEntry(entry.Name()) {
					continue
				}

				return true, nil
			}

			found, err := hasFragment(filepath.Join(dir, entry.Name()))
			if err != nil {
				return false, err
			}

			if found {
				return true, nil
			}
		}

		if len(entries) < dirBatch {
			return false, nil
		}
	}
}

// List returns the fragment names on a disk with the given slash-separated
// prefix, sorted lexicographically. It is store-local (not part of the
// transport API) — the scrubber and repair worker enumerate their own node's
// fragments with it. In-flight temp files are skipped.
func (s *Store) List(_ context.Context, disk cluster.DiskID, prefix string) ([]string, error) {
	root, ok := s.roots[disk]
	if !ok {
		return nil, errors.Errorf("unknown disk %q", disk)
	}

	var names []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				// Racing a delete+prune is fine.
				return nil
			}

			return err
		}

		if d.IsDir() || skipEntry(d.Name()) {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return errors.Wrap(err, "relativize fragment path")
		}

		if name := filepath.ToSlash(rel); strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}

		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "walk fragments")
	}

	sort.Strings(names)

	return names, nil
}

// fileWriter stages a fragment and commits it on Close: fsync per policy,
// rename into place, parent-dir fsync per policy. Close after a failed write
// still commits the bytes received so far (matching MemStore) — the
// coordinator's sidecar protocol never exposes a torn fragment and deletes it
// on a refused write.
type fileWriter struct {
	store *Store
	disk  cluster.DiskID
	tmp   *os.File
	path  string
	// n is what landed in the temp file, which is what the index counts once
	// the rename commits it.
	n int64
}

func (w *fileWriter) Write(p []byte) (int, error) {
	n, err := w.tmp.Write(p)
	w.n += int64(n)

	return n, err //nolint:wrapcheck // Pass the write error through untouched.
}

func (w *fileWriter) Close() error {
	if err := w.syncFile(); err != nil {
		w.abort()
		return err
	}

	if err := w.tmp.Close(); err != nil {
		_ = os.Remove(w.tmp.Name())
		return errors.Wrap(err, "close temp file")
	}

	// What this rename is about to displace. A fragment name is
	// generation-stamped and written once, but the sidecar under it is
	// replaced on every commit, so overwriting is the common path and the
	// index would drift upward without this.
	prev, existed := fileSize(w.path)

	if err := os.Rename(w.tmp.Name(), w.path); err != nil {
		_ = os.Remove(w.tmp.Name())
		return errors.Wrap(err, "rename fragment into place")
	}

	w.store.index[w.disk].added(w.n, prev, existed)

	return w.syncDir()
}

// fileSize reports an existing file's size. A missing file is not an error
// here: it is the answer.
func fileSize(path string) (size int64, ok bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0, false
	}

	return info.Size(), true
}

// abort discards the temp file after a failed commit step.
func (w *fileWriter) abort() {
	_ = w.tmp.Close()
	_ = os.Remove(w.tmp.Name())
}

// syncFile fsyncs the staged payload when the policy requires file-level
// durability.
func (w *fileWriter) syncFile() error {
	if w.store.sync < storagefs.SyncFile {
		return nil
	}

	if err := w.tmp.Sync(); err != nil {
		return errors.Wrap(err, "fsync fragment")
	}

	return nil
}

// syncDir fsyncs the fragment's directory after the rename so its visibility
// survives a crash. Like storagefs, a no-op on Windows (directory handles
// cannot be synced there; NTFS journals directory metadata).
func (w *fileWriter) syncDir() error {
	if w.store.sync < storagefs.SyncFileDir {
		return nil
	}

	return syncDir(filepath.Dir(w.path))
}
