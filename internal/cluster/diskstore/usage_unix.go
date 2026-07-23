//go:build unix

package diskstore

import "golang.org/x/sys/unix"

// fsUsage reads filesystem capacity via statfs.
func fsUsage(root string) (DiskUsage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return DiskUsage{}, err
	}

	bsize := uint64(st.Bsize) //nolint:gosec // Block size is positive; its Go type varies by platform.

	return DiskUsage{
		TotalBytes: st.Blocks * bsize,
		FreeBytes:  st.Bavail * bsize,
	}, nil
}
