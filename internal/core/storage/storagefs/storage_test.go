package storagefs_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/storage/storagefs"
)

func newStorage(t testing.TB) *storagefs.Storage {
	t.Helper()

	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	return storage
}
