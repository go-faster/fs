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

// TestAdoptedSurvivesOpen is the signal Open used to destroy.
//
// Open must record the index as building before serving a write, so a process
// that dies leaves the next start something to rebuild from. That overwrite is
// also the only record of how the *last* process ended, so reading State after
// Open always said "building" — and every restart re-walked every disk, which
// is the scan the index exists to avoid.
func TestAdoptedSurvivesOpen(t *testing.T) {
	dir := t.TempDir()

	idx, err := Open(dir)
	require.NoError(t, err)
	assert.False(t, idx.Adopted(), "a fresh index was handed over by nobody")

	require.NoError(t, idx.Put(t.Context(), metastore.Entry{Bucket: "photos", Key: "a.jpg", Size: 100}))
	require.NoError(t, idx.MarkReady(t.Context()))
	require.NoError(t, idx.Close())

	clean, err := Open(dir)
	require.NoError(t, err)
	assert.True(t, clean.Adopted(), "a clean close is adoptable")

	// And the persisted state is still building, because a crash from here
	// must schedule a rebuild. Both things are true at once, which is why the
	// answer has to be carried separately from State.
	state, err := clean.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state)

	// A process that dies without closing leaves nothing to adopt.
	require.NoError(t, clean.db.Close())

	crashed, err := Open(dir)
	require.NoError(t, err)

	t.Cleanup(func() { _ = crashed.Close() })

	assert.False(t, crashed.Adopted(), "an unclean stop is rebuilt, not adopted")
}

// TestOlderVersionIsNotAdopted: a clean close at a shape this binary cannot
// read is still a rebuild. The two reasons are independent and either is
// enough.
func TestOlderVersionIsNotAdopted(t *testing.T) {
	dir := t.TempDir()

	idx, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, idx.setEntryVersion(entryVersion-1))
	require.NoError(t, idx.MarkReady(t.Context()))
	require.NoError(t, idx.Close())

	reopened, err := Open(dir)
	require.NoError(t, err)

	t.Cleanup(func() { _ = reopened.Close() })

	assert.False(t, reopened.Adopted(),
		"a clean close in an older shape is not something to trust")
}
