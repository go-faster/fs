//go:build windows

package clusterstore

import "os"

// discardSpool defers removal of the spool file to cleanup.
//
// Windows cannot do what the POSIX path does: a file that is still open cannot
// be deleted (Go opens without FILE_SHARE_DELETE), so the name has to outlive
// the descriptor and removing it is cleanup's job. A process killed mid-upload
// therefore leaves the file behind, in the temp directory, where the OS sweeps
// it — that is the platform's terms, not a choice.
func discardSpool(f *os.File) (cleanup func(), err error) {
	name := f.Name()

	return func() {
		_ = f.Close()
		_ = os.Remove(name)
	}, nil
}
