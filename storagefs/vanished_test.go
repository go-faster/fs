package storagefs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// The two narrowing conditions in vanished are the whole of it: it exists to
// turn one specific failure into one specific answer, and everything it does
// *not* convert matters as much as what it does. A "not found" for a full disk
// or a permission problem would be a lie in the quieter direction — the client
// would go and look for a bucket that is sitting right there.

func TestVanishedReportsADeletedBucket(t *testing.T) {
	root := t.TempDir()
	s := &Storage{root: root}

	gone := filepath.Join(root, "gone")

	err := s.vanished(gone, errors.Wrap(os.ErrNotExist, "rename object"))
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
}

func TestVanishedKeepsAMissingPathWhoseBucketIsThere(t *testing.T) {
	root := t.TempDir()
	s := &Storage{root: root}

	present := filepath.Join(root, "present")
	require.NoError(t, os.MkdirAll(present, 0o750))

	original := errors.Wrap(os.ErrNotExist, "rename object")

	err := s.vanished(present, original)
	assert.Equal(t, original, err, "a missing path became a missing bucket that exists")
	assert.NotErrorIs(t, err, fs.ErrBucketNotFound)
}

func TestVanishedKeepsAnUnrelatedFailure(t *testing.T) {
	root := t.TempDir()
	s := &Storage{root: root}

	// The bucket really is gone, and the failure still has nothing to do with
	// it: a disk that is full does not become a bucket that is missing.
	gone := filepath.Join(root, "gone")
	original := errors.Wrap(os.ErrPermission, "rename object")

	err := s.vanished(gone, original)
	assert.Equal(t, original, err)
	assert.NotErrorIs(t, err, fs.ErrBucketNotFound)
}
