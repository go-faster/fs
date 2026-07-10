package storagefs_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagefs"
	"github.com/go-faster/fs/storagetest"
)

func TestStorageConformance(t *testing.T) {
	t.Parallel()

	storagetest.Run(t, func(t testing.TB) fs.Storage {
		storage, err := storagefs.New(t.TempDir())
		require.NoError(t, err)

		return storage
	})
}
