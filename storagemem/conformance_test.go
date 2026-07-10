package storagemem_test

import (
	"testing"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagemem"
	"github.com/go-faster/fs/storagetest"
)

func TestStorageConformance(t *testing.T) {
	t.Parallel()

	storagetest.Run(t, func(testing.TB) fs.Storage {
		return storagemem.New()
	})
}
