package objindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/metastore"
)

// TestOlderEntryVersionForcesARebuild is what the version stamp is for, and it
// had never been exercised — the stamp existed but had never been bumped, so
// nothing had ever depended on it working.
//
// Entries written in an older shape are not adopted. They may even decode
// cleanly, as version 2's do into version 3's Locator-carrying Entry, and that
// is exactly why this must not be judged case by case: a shape change that
// happens to decode is indistinguishable from one that does not until it is
// wrong in production, and rebuilding is cheap because the index is derived.
//
// In package rather than beside the other tests because the version marker is
// deliberately unexported: nothing outside should be able to claim an index is
// current.
func TestOlderEntryVersionForcesARebuild(t *testing.T) {
	dir := t.TempDir()

	idx, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, idx.Put(t.Context(), metastore.Entry{Bucket: "photos", Key: "a.jpg", Size: 100}))
	require.NoError(t, idx.MarkReady(t.Context()))
	require.NoError(t, idx.Close())

	// A clean close leaves it adoptable, which is what makes the next assertion
	// about the version and not about the shutdown.
	stale, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, stale.setEntryVersion(entryVersion-1))
	require.NoError(t, stale.MarkReady(t.Context()))
	require.NoError(t, stale.Close())

	upgraded, err := Open(dir)
	require.NoError(t, err)

	t.Cleanup(func() { _ = upgraded.Close() })

	state, err := upgraded.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state,
		"an index written in an older shape is rebuilt, not adopted")

	// And the stamp is corrected on the way through, so the rebuild is owed
	// once rather than on every start.
	stored, err := upgraded.entryVersion()
	require.NoError(t, err)
	assert.Equal(t, entryVersion, stored)
}

// TestCurrentEntryVersionIsAdopted is the other half: a clean close at the
// current version must still be adoptable, or every restart pays for a full
// rebuild of an index that was correct.
func TestCurrentEntryVersionIsAdopted(t *testing.T) {
	dir := t.TempDir()

	idx, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, idx.MarkReady(t.Context()))
	require.NoError(t, idx.Close())

	reopened, err := Open(dir)
	require.NoError(t, err)

	t.Cleanup(func() { _ = reopened.Close() })

	stored, err := reopened.entryVersion()
	require.NoError(t, err)
	assert.Equal(t, entryVersion, stored, "a current index keeps its stamp")
}
