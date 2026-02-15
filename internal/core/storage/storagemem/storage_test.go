package storagemem_test

import (
	"testing"

	"github.com/go-faster/fs/internal/core/storage/storagemem"
)

func newStorage(t testing.TB) *storagemem.Storage {
	t.Helper()
	return storagemem.New()
}
