package diskstore

import (
	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// DiskUsage is one disk root's filesystem capacity snapshot.
type DiskUsage struct {
	// TotalBytes is the filesystem size backing the disk root.
	TotalBytes uint64
	// FreeBytes is the space available to unprivileged writes.
	FreeBytes uint64
}

// Usage reports the capacity of one disk's backing filesystem.
func (s *Store) Usage(disk cluster.DiskID) (DiskUsage, error) {
	root, ok := s.roots[disk]
	if !ok {
		return DiskUsage{}, errors.Errorf("unknown disk %q", disk)
	}

	u, err := fsUsage(root)
	if err != nil {
		return DiskUsage{}, errors.Wrapf(err, "stat filesystem of disk %q", disk)
	}

	return u, nil
}
