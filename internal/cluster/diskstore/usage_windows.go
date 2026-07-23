//go:build windows

package diskstore

import "golang.org/x/sys/windows"

// fsUsage reads filesystem capacity via GetDiskFreeSpaceEx.
func fsUsage(root string) (DiskUsage, error) {
	path, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return DiskUsage{}, err
	}

	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &free, &total, &totalFree); err != nil {
		return DiskUsage{}, err
	}

	return DiskUsage{TotalBytes: total, FreeBytes: free}, nil
}
