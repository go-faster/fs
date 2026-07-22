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

	s := &Store{roots: make(map[cluster.DiskID]string, len(roots))}

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
	}

	for _, o := range opts {
		o(s)
	}

	return s, nil
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

	return &fileWriter{store: s, tmp: tmp, path: path}, nil
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

	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return transport.ErrNotFound
	}

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return transport.ErrNotFound
		}

		return errors.Wrap(err, "delete fragment")
	}

	s.pruneEmptyDirs(filepath.Dir(path), s.roots[disk])

	return nil
}

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

		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
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
	tmp   *os.File
	path  string
}

func (w *fileWriter) Write(p []byte) (int, error) { return w.tmp.Write(p) }

func (w *fileWriter) Close() error {
	if err := w.syncFile(); err != nil {
		w.abort()
		return err
	}

	if err := w.tmp.Close(); err != nil {
		_ = os.Remove(w.tmp.Name())
		return errors.Wrap(err, "close temp file")
	}

	if err := os.Rename(w.tmp.Name(), w.path); err != nil {
		_ = os.Remove(w.tmp.Name())
		return errors.Wrap(err, "rename fragment into place")
	}

	return w.syncDir()
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
