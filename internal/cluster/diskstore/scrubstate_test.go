package diskstore_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
)

// The disk store must satisfy the interface the repairer declares; nothing else
// checks this, since neither package imports the other.
var _ clusterstore.ScrubStateStore = diskstore.ScrubStateStore{}

func TestScrubStateRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d0")

	s, err := diskstore.New(map[cluster.DiskID]string{"d0": root})
	require.NoError(t, err)

	store := s.ScrubStateStore()

	// Nothing recorded yet: no pass in flight, no coverage claimed.
	empty := store.LoadScrubState("d0")
	assert.False(t, empty.InProgress())
	assert.True(t, empty.LastCompleted.IsZero())

	started := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, store.SaveScrubState("d0", cluster.ScrubState{
		Cursor:      "obj/aa/bb",
		PassStarted: started,
	}))

	got := store.LoadScrubState("d0")
	assert.Equal(t, "obj/aa/bb", got.Cursor)
	assert.True(t, got.InProgress())
	assert.True(t, started.Equal(got.PassStarted))

	// A completed pass clears the cursor and stamps coverage.
	completed := started.Add(time.Hour)

	require.NoError(t, store.SaveScrubState("d0", cluster.ScrubState{LastCompleted: completed}))

	got = store.LoadScrubState("d0")
	assert.False(t, got.InProgress())
	assert.True(t, completed.Equal(got.LastCompleted))

	// The cursor survives a restart, which is the entire point.
	reopened, err := diskstore.New(map[cluster.DiskID]string{"d0": root})
	require.NoError(t, err)
	assert.True(t, completed.Equal(reopened.ScrubStateStore().LoadScrubState("d0").LastCompleted))
}

// TestScrubStateUnknownDisk: an unknown disk reads as "nothing recorded" and
// refuses to write, rather than inventing a path.
func TestScrubStateUnknownDisk(t *testing.T) {
	s, err := diskstore.New(map[cluster.DiskID]string{"d0": filepath.Join(t.TempDir(), "d0")})
	require.NoError(t, err)

	store := s.ScrubStateStore()

	assert.False(t, store.LoadScrubState("nope").InProgress())
	require.Error(t, store.SaveScrubState("nope", cluster.ScrubState{Cursor: "x"}))
}

// TestScrubStateIsNotAFragment is the guard that matters: the cursor file lives
// at the disk root, in the tree the fragment walkers scan. If it counted as a
// fragment, a drained disk would report occupied forever and the decommission
// waiting on has_data would never finish.
func TestScrubStateIsNotAFragment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d0")

	s, err := diskstore.New(map[cluster.DiskID]string{"d0": root})
	require.NoError(t, err)

	require.NoError(t, s.ScrubStateStore().SaveScrubState("d0", cluster.ScrubState{Cursor: "obj/aa"}))
	require.NoError(t, s.ScanAll(t.Context()))

	held, err := s.HasData(t.Context(), "d0")
	require.NoError(t, err)
	assert.False(t, held, "the scrub cursor is bookkeeping, not data")

	names, err := s.List(t.Context(), "d0", "")
	require.NoError(t, err)
	assert.Empty(t, names, "nor does it list as a fragment")

	occupancy, err := s.Occupancy("d0")
	require.NoError(t, err)
	assert.Zero(t, occupancy.Fragments, "nor is it counted")
}
