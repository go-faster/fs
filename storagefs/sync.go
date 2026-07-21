package storagefs

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/go-faster/errors"
)

// SyncPolicy controls how aggressively storagefs flushes writes to stable
// storage. Atomicity (no torn object ever visible) comes from the temp-file +
// rename protocol regardless of policy; SyncPolicy governs durability — whether
// an acknowledged write survives a power loss.
type SyncPolicy int

const (
	// SyncNone performs no fsync. Fastest; an acked write may be lost on power
	// loss (never torn). The library/test default.
	SyncNone SyncPolicy = iota
	// SyncFile fsyncs file contents before the rename. The object's data is
	// durable once PutObject returns; the directory entry (the rename) relies on
	// the OS to persist it.
	SyncFile
	// SyncFileDir fsyncs file contents and the parent directory after the
	// rename, so both the data and its visibility survive a crash. The binary
	// default.
	SyncFileDir
)

// ParseSyncPolicy maps a config string to a SyncPolicy, defaulting unknown or
// empty values to SyncFileDir (the safe choice).
func ParseSyncPolicy(s string) (SyncPolicy, error) {
	switch s {
	case "none":
		return SyncNone, nil
	case "file":
		return SyncFile, nil
	case "file+dir", "":
		return SyncFileDir, nil
	default:
		return SyncNone, errors.Errorf("invalid sync policy %q (want none, file or file+dir)", s)
	}
}

// Option configures a Storage.
type Option func(*Storage)

// WithSyncPolicy sets the durability policy (default SyncNone).
func WithSyncPolicy(p SyncPolicy) Option {
	return func(s *Storage) { s.sync = p }
}

// syncFile fsyncs f when the policy requires file-level durability.
func (s *Storage) syncFile(f *os.File) error {
	if s.sync < SyncFile {
		return nil
	}

	if err := f.Sync(); err != nil {
		return errors.Wrap(err, "fsync file")
	}

	return nil
}

// syncDir fsyncs dir when the policy requires directory-level durability, so a
// rename into it is persisted. Windows does not support syncing a directory
// handle (NTFS journals directory metadata), so it is a no-op there — the
// file+dir policy degrades to file-level durability on Windows.
func (s *Storage) syncDir(dir string) error {
	if s.sync < SyncFileDir || runtime.GOOS == "windows" {
		return nil
	}

	d, err := os.Open(dir) //nolint:gosec // Path is an internal storage directory.
	if err != nil {
		return errors.Wrap(err, "open dir for sync")
	}

	if err := d.Sync(); err != nil {
		_ = d.Close()
		return errors.Wrap(err, "fsync dir")
	}

	if err := d.Close(); err != nil {
		return errors.Wrap(err, "close dir")
	}

	return nil
}

// atomicWrite writes data to path via a temp file, fsync (per policy), rename,
// and parent-dir fsync (per policy), so the file appears atomically and — under
// SyncFile/SyncFileDir — durably.
func (s *Storage) atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return errors.Wrap(err, "create temp file")
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())

		return errors.Wrap(err, "write temp file")
	}

	if err := s.syncFile(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())

		return err
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return errors.Wrap(err, "close temp file")
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return errors.Wrap(err, "rename temp file")
	}

	return s.syncDir(dir)
}
