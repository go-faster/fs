package clusterstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/metastore"
)

// storeKeys drains the test bucket out of the store, so a rebuild can be
// compared against what the disks hold.
func storeKeys(t *testing.T, store metastore.Store) []string {
	t.Helper()

	var keys []string

	require.NoError(t, store.Scan(t.Context(), "b", "", "", 0, func(e metastore.Entry) error {
		keys = append(keys, e.Key)

		return nil
	}))

	return keys
}

// TestRebuildFillsTheStoreFromTheDisks: the sidecars are the commit point, so a
// rebuild reads them and the store ends up holding exactly what they say.
func TestRebuildFillsTheStoreFromTheDisks(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, store := clusterCoordinator(t, fc)

	want := []string{"a.txt", "b.txt", "docs/one.txt", "z.txt"}
	for _, key := range want {
		mustPut(t, c, key, randBytes(32))
	}

	c.Flush()

	report, err := c.RebuildMetadata(t.Context(), RebuildOptions{})
	require.NoError(t, err)

	assert.Equal(t, len(want), report.Objects)
	assert.Equal(t, want, storeKeys(t, store))

	state, err := store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateReady, state, "a completed rebuild makes the store usable")
}

// TestRebuildLeavesTheStoreUnreadableUntilItFinishes: marking ready early would
// serve a listing that reports a fraction of the cluster as all of it, which is
// the one outcome worse than refusing to answer.
func TestRebuildLeavesTheStoreUnreadableUntilItFinishes(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, store := clusterCoordinator(t, fc)

	for i := range 5 {
		mustPut(t, c, fmt.Sprintf("k%02d", i), randBytes(16))
	}

	c.Flush()

	stop := assert.AnError

	_, err := c.RebuildMetadata(t.Context(), RebuildOptions{
		Checkpoint: func(context.Context, RebuildCursor) error { return stop },
	})
	require.ErrorIs(t, err, stop)

	state, err := store.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state)

	// And the listing refuses rather than answering short.
	objects, _, _, err := c.ListPage(t.Context(), "b", "", "", "", 0)
	require.ErrorIs(t, err, ErrIndexUnavailable)
	require.Empty(t, objects)
}

// TestRebuildResumesWithoutDuplicatesOrGaps is the acceptance criterion, and
// the case the cursor exists for: a runner is killed mid-walk, a standby picks
// up from the checkpoint, and the store ends up holding every key exactly once.
func TestRebuildResumesWithoutDuplicatesOrGaps(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, store := clusterCoordinator(t, fc)

	var want []string

	for i := range 12 {
		key := fmt.Sprintf("k%02d", i)
		mustPut(t, c, key, randBytes(16))

		want = append(want, key)
	}

	c.Flush()

	// First runner: dies at its first checkpoint, having indexed some prefix of
	// the bucket. Checkpointing every object rather than every batch is what
	// makes the kill land mid-bucket deterministically.
	var (
		cursor RebuildCursor
		seen   int
	)

	_, err := c.RebuildMetadata(t.Context(), RebuildOptions{
		Checkpoint: func(_ context.Context, cur RebuildCursor) error { return nil },
		OnObject: func(bucket, key string) {
			seen++
			if seen == 5 {
				cursor = RebuildCursor{Bucket: bucket, Key: key}
			}
		},
	})
	require.NoError(t, err)
	require.Equal(t, "k04", cursor.Key, "the kill point is where the standby resumes from")

	// Second runner: takes over from the cursor. It must not reset — doing so
	// would discard the first runner's work and make every leadership change a
	// restart.
	report, err := c.RebuildMetadata(t.Context(), RebuildOptions{
		Resume:   cursor,
		Resuming: true,
	})
	require.NoError(t, err)

	assert.Equal(t, len(want)-5, report.Objects, "the resume re-indexes only what was left")
	assert.Equal(t, want, storeKeys(t, store), "every key exactly once, in order")
}

// TestRebuildResumingDoesNotEmptyTheStore is the same rule stated directly,
// because it is the one that turns a large cluster's rebuild into a loop that
// never converges if it is wrong.
func TestRebuildResumingDoesNotEmptyTheStore(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, store := clusterCoordinator(t, fc)

	for _, key := range []string{"a", "b", "c"} {
		mustPut(t, c, key, randBytes(16))
	}

	c.Flush()

	// Pretend a previous runner had already indexed "a".
	require.NoError(t, store.Put(t.Context(), metastore.Entry{Bucket: "b", Key: "a", Size: 16}))

	_, err := c.RebuildMetadata(t.Context(), RebuildOptions{
		Resume:   RebuildCursor{Bucket: "b", Key: "a"},
		Resuming: true,
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"a", "b", "c"}, storeKeys(t, store),
		"the resumed rebuild kept what was already there")
}

// TestRebuildFromScratchEmptiesFirst: an object deleted while nothing was
// watching leaves an entry that adding alone would never remove, and a store
// listing what is gone is worse than one merely behind.
func TestRebuildFromScratchEmptiesFirst(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, store := clusterCoordinator(t, fc)

	mustPut(t, c, "real.txt", randBytes(16))
	c.Flush()

	// A key the disks do not have — the shape a delete missed while the store
	// was not watching leaves behind.
	require.NoError(t, store.Put(t.Context(), metastore.Entry{Bucket: "b", Key: "ghost.txt", Size: 1}))

	_, err := c.RebuildMetadata(t.Context(), RebuildOptions{})
	require.NoError(t, err)

	assert.Equal(t, []string{"real.txt"}, storeKeys(t, store))
}

// TestRebuildCheckpointsProgress: the cursor has to move, or a killed runner
// starts over and a cluster large enough to need this never finishes one.
func TestRebuildCheckpointsProgress(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, _ := clusterCoordinator(t, fc)

	for i := range 3 {
		mustPut(t, c, fmt.Sprintf("k%d", i), randBytes(16))
	}

	c.Flush()

	var cursors []RebuildCursor

	_, err := c.RebuildMetadata(t.Context(), RebuildOptions{
		Checkpoint: func(_ context.Context, cur RebuildCursor) error {
			cursors = append(cursors, cur)

			return nil
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, cursors, "a rebuild that never checkpoints cannot be resumed")
	assert.Equal(t, RebuildCursor{Bucket: "b", Key: "k2"}, cursors[len(cursors)-1],
		"the last checkpoint names the last key indexed")
}

// TestRebuildRefusesWithoutAClusterScopeStore: at local scope a node rebuilds
// its own index from its own disks, which is a different mechanism entirely and
// must not be reached through this one.
func TestRebuildRefusesWithoutAClusterScopeStore(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, _ := indexedCoordinator(t, fc)

	_, err := c.RebuildMetadata(t.Context(), RebuildOptions{})
	require.Error(t, err)
}

// TestRebuildCursorRoundTrip: the cursor is persisted in etcd, so it has to
// survive encoding.
func TestRebuildCursorRoundTrip(t *testing.T) {
	want := RebuildCursor{Bucket: "photos", Key: "a/b/c.jpg"}

	raw, err := want.Encode()
	require.NoError(t, err)

	got, err := DecodeRebuildCursor(raw)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
