package diskstore

import (
	"os"
	"runtime"

	"github.com/go-faster/errors"
)

// syncDir fsyncs a directory so a rename into it is persisted. Mirrors
// storagefs: Windows cannot sync a directory handle, so the file+dir policy
// degrades to file-level durability there.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	d, err := os.Open(dir) //nolint:gosec // Internal storage directory.
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
