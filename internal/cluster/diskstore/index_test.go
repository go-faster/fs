package diskstore_test

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
)

// occupancy is the index readout for a disk, asserted anchored: an unanchored
// zero means "not counted yet" and would pass most of these assertions while
// meaning nothing.
func occupancy(t *testing.T, s *diskstore.Store, disk cluster.DiskID) diskstore.Occupancy {
	t.Helper()

	u, err := s.Occupancy(disk)
	require.NoError(t, err)
	require.True(t, u.Anchored, "counters must be anchored by a scan or an adopted checkpoint")

	return u
}

func TestOccupancyTracksWrites(t *testing.T) {
	s := newStore(t, "d0", "d1")

	require.NoError(t, s.ScanAll(t.Context()))

	empty := occupancy(t, s, "d0")
	assert.Zero(t, empty.Fragments)
	assert.Zero(t, empty.Bytes)
	assert.True(t, empty.Empty(), "a scanned, untouched disk is empty")

	put(t, s, "d0", "obj/aa/f0", make([]byte, 100))
	put(t, s, "d0", "obj/aa/f1", make([]byte, 250))

	u := occupancy(t, s, "d0")
	assert.EqualValues(t, 2, u.Fragments)
	assert.EqualValues(t, 350, u.Bytes)
	assert.False(t, u.Empty())

	// Disks are counted separately, or a drain of one would wait on the other.
	other := occupancy(t, s, "d1")
	assert.Zero(t, other.Fragments)

	require.NoError(t, s.Delete(t.Context(), "d0", "obj/aa/f0"))

	u = occupancy(t, s, "d0")
	assert.EqualValues(t, 1, u.Fragments)
	assert.EqualValues(t, 250, u.Bytes)

	require.NoError(t, s.Delete(t.Context(), "d0", "obj/aa/f1"))

	u = occupancy(t, s, "d0")
	assert.True(t, u.Empty(), "the index reaches zero, which is what a drain waits on")
}

// TestOccupancyOverwriteCountsOnce covers the sidecar path: the same name is
// rewritten on every commit, so an index that only added would climb forever
// on a bucket that never grows.
func TestOccupancyOverwriteCountsOnce(t *testing.T) {
	s := newStore(t, "d0")
	require.NoError(t, s.ScanAll(t.Context()))

	put(t, s, "d0", "obj/aa/meta", make([]byte, 500))
	put(t, s, "d0", "obj/aa/meta", make([]byte, 120))

	u := occupancy(t, s, "d0")
	assert.EqualValues(t, 1, u.Fragments, "a replaced fragment is still one fragment")
	assert.EqualValues(t, 120, u.Bytes, "the replaced size is displaced, not added")
}

// TestOccupancyUnanchoredBeforeScan checks that a fresh store reports "not
// counted" rather than zero. Reading unanchored counters as a drained disk is
// how a volume still holding data gets deleted.
func TestOccupancyUnanchoredBeforeScan(t *testing.T) {
	s := newStore(t, "d0")

	u, err := s.Occupancy("d0")
	require.NoError(t, err)
	assert.False(t, u.Anchored)
	assert.False(t, u.Empty(), "unanchored is unknown, never empty")

	_, err = s.Occupancy("nope")
	require.Error(t, err, "an unknown disk is an error, not an empty reading")
}

// TestOccupancyScanAnchorsExistingData covers the case the checkpoint cannot:
// data written by a previous process, found by walking.
func TestOccupancyScanAnchorsExistingData(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d0")
	roots := map[cluster.DiskID]string{"d0": root}

	first, err := diskstore.New(roots)
	require.NoError(t, err)
	require.NoError(t, first.ScanAll(t.Context()))

	for i := range 5 {
		put(t, first, "d0", "obj/aa/f"+strconv.Itoa(i), make([]byte, 10))
	}

	// A second store over the same root, never closed by the first: no
	// checkpoint to adopt, so it starts unanchored and the scan is what
	// establishes the truth.
	second, err := diskstore.New(roots)
	require.NoError(t, err)

	before, err := second.Occupancy("d0")
	require.NoError(t, err)
	require.False(t, before.Anchored)

	require.NoError(t, second.Scan(t.Context(), "d0"))

	u := occupancy(t, second, "d0")
	assert.EqualValues(t, 5, u.Fragments)
	assert.EqualValues(t, 50, u.Bytes)
	assert.False(t, u.ScannedAt.IsZero())
}

// TestOccupancyCheckpointRoundTrip checks the point of persisting at all: a
// clean restart adopts the counters and serves them before any scan runs.
func TestOccupancyCheckpointRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d0")
	roots := map[cluster.DiskID]string{"d0": root}

	first, err := diskstore.New(roots)
	require.NoError(t, err)
	require.NoError(t, first.ScanAll(t.Context()))

	put(t, first, "d0", "obj/aa/f0", make([]byte, 64))
	put(t, first, "d0", "obj/bb/f0", make([]byte, 36))

	require.NoError(t, first.Close())

	second, err := diskstore.New(roots)
	require.NoError(t, err)

	u := occupancy(t, second, "d0")
	assert.EqualValues(t, 2, u.Fragments, "adopted without walking the disk")
	assert.EqualValues(t, 100, u.Bytes)
}

// TestOccupancyCheckpointNotAdoptedAfterCrash is the other half of the
// contract: counters that were not handed over cleanly are refused. Adopting
// them would report a stale total as fact, and the staleness is unbounded —
// everything written after the last checkpoint is missing from it.
func TestOccupancyCheckpointNotAdoptedAfterCrash(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d0")
	roots := map[cluster.DiskID]string{"d0": root}

	first, err := diskstore.New(roots)
	require.NoError(t, err)
	require.NoError(t, first.ScanAll(t.Context()))
	require.NoError(t, first.Close())

	// Reopened (which invalidates the checkpoint) and then lost without a
	// Close — the crash case.
	crashed, err := diskstore.New(roots)
	require.NoError(t, err)

	put(t, crashed, "d0", "obj/aa/f0", make([]byte, 7))

	restarted, err := diskstore.New(roots)
	require.NoError(t, err)

	u, err := restarted.Occupancy("d0")
	require.NoError(t, err)
	assert.False(t, u.Anchored, "an unclean checkpoint schedules a scan instead of being believed")

	require.NoError(t, restarted.Scan(t.Context(), "d0"))

	scanned := occupancy(t, restarted, "d0")
	assert.EqualValues(t, 1, scanned.Fragments)
	assert.EqualValues(t, 7, scanned.Bytes)
}

// TestIndexFileIsNotAFragment guards the interaction that would break the
// drain gate: the checkpoint lives at the disk root, and every fragment walker
// has to treat it as bookkeeping. If HasData counted it, a drained disk would
// never report empty and a decommission would wait forever.
func TestIndexFileIsNotAFragment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d0")
	roots := map[cluster.DiskID]string{"d0": root}

	s, err := diskstore.New(roots)
	require.NoError(t, err)
	require.NoError(t, s.ScanAll(t.Context()))
	require.NoError(t, s.Close())

	// The checkpoint is on disk now.
	entries, err := filepath.Glob(filepath.Join(root, ".usage.json"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the checkpoint is written at the disk root")

	held, err := s.HasData(t.Context(), "d0")
	require.NoError(t, err)
	assert.False(t, held, "the checkpoint is not data")

	names, err := s.List(t.Context(), "d0", "")
	require.NoError(t, err)
	assert.Empty(t, names, "nor does it list as a fragment")

	require.NoError(t, s.Scan(t.Context(), "d0"))

	u := occupancy(t, s, "d0")
	assert.Zero(t, u.Fragments, "nor is it counted")
	assert.Zero(t, u.Bytes)
}
