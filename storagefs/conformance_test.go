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

	// Known gaps, left to the engine that replaces this backend (#277): the
	// null version is read from the plain path only when no version exists,
	// and a write while suspended does not become the current version.
	storagetest.RunExcept(t, func(t testing.TB) fs.Storage {
		storage, err := storagefs.New(t.TempDir())
		require.NoError(t, err)

		return storage
	}, map[string]string{
		"Versioning/NullBeforeEnable": "storagefs serves the current object for versionId=null once versions exist (#277)",
		"Versioning/Suspended":        "storagefs does not make a suspended write current (#277)",
	})
}
