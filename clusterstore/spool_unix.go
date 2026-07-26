//go:build !windows

package clusterstore

import (
	"os"

	"github.com/go-faster/errors"
)

// discardSpool unlinks the spool file immediately and returns a cleanup that
// only closes it.
//
// The open descriptor keeps the data reachable while the name is already gone,
// so there is no window in which a crash leaks the file: the kernel frees the
// blocks when the last descriptor closes, however the process ends.
func discardSpool(f *os.File) (cleanup func(), err error) {
	if err := os.Remove(f.Name()); err != nil {
		return nil, errors.Wrap(err, "unlink spool file")
	}

	return func() { _ = f.Close() }, nil
}
