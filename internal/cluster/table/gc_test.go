package table

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

func queued(t *testing.T, n *node) int {
	t.Helper()

	var c int

	require.NoError(t, n.table.db.View(func(tx *bbolt.Tx) error {
		c = tx.Bucket(n.table.gcBucket()).Stats().KeyN

		return nil
	}))

	return c
}

// everywhere waits for every node to hold n rows: writes return at a quorum
// of two, and the third replica's copy lands after.
func everywhere(t *testing.T, nodes []*node, n int) {
	t.Helper()

	require.Eventually(t, func() bool {
		for _, nd := range nodes {
			if len(local(t, nd)) != n {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)
}

func TestCollect(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)

	require.NoError(t, nodes[0].table.Insert(ctx, "b", "deleted", reg{V: "gone", TS: 1}))
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "trimmed", reg{V: "x+tomb", TS: 1}))
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "live", reg{V: "x", TS: 1}))

	everywhere(t, nodes, 3)

	require.NoError(t, nodes[0].table.Collect(ctx, time.Hour))
	assert.Len(t, local(t, nodes[1]), 3, "nothing is due before the delay")

	require.NoError(t, nodes[0].table.Collect(ctx, 0))

	for _, n := range nodes {
		assert.Equal(t, map[string]string{
			"b/live":    `{"v":"x","ts":1}`,
			"b/trimmed": `{"v":"x","ts":1}`,
		}, local(t, n))
		assert.Zero(t, queued(t, n), "every replica's queue entry is about the row replaced")
	}

	assert.Equal(t, int64(2), nodes[0].table.GCStats().Collected)

	// Another replica's pass finds nothing left.
	require.NoError(t, nodes[1].table.Collect(ctx, 0))
	assert.Zero(t, nodes[1].table.GCStats().Collected)
}

func TestCollectDefersWithoutEveryReplica(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)

	require.NoError(t, nodes[0].table.Insert(ctx, "b", "k", reg{V: "gone", TS: 1}))
	everywhere(t, nodes, 1)

	nodes[2].srv.Close()

	require.NoError(t, nodes[0].table.Collect(ctx, 0))
	assert.Len(t, local(t, nodes[0]), 1, "a tombstone stays until every replica can drop it")
	assert.Len(t, local(t, nodes[1]), 1)
	assert.Equal(t, int64(1), nodes[0].table.GCStats().Deferred)
	assert.Equal(t, 1, queued(t, nodes[0]), "and is tried again")
}

func TestCollectKeepsNewerWrite(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)

	require.NoError(t, nodes[0].table.Insert(ctx, "b", "k", reg{V: "gone", TS: 1}))
	everywhere(t, nodes, 1)

	// A newer write reaches one replica only, after the tombstone was queued.
	require.NoError(t, nodes[2].table.localInsert([]wireEntry{row("b", "k", reg{V: "back", TS: 2})}))

	require.NoError(t, nodes[0].table.Collect(ctx, 0))
	assert.Empty(t, local(t, nodes[0]))
	assert.Equal(t, map[string]string{"b/k": `{"v":"back","ts":2}`}, local(t, nodes[2]), "the newer write survives")

	require.NoError(t, nodes[0].table.Sync(ctx))
	assert.Equal(t, map[string]string{"b/k": `{"v":"back","ts":2}`}, local(t, nodes[0]), "and anti-entropy restores it")
}

func TestCollectDropsStaleQueueEntries(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)

	require.NoError(t, nodes[0].table.localInsert([]wireEntry{row("b", "k", reg{V: "gone", TS: 1})}))
	require.Equal(t, 1, queued(t, nodes[0]))

	// The tombstone is overwritten by a live row: nothing to collect.
	require.NoError(t, nodes[0].table.localInsert([]wireEntry{row("b", "k", reg{V: "live", TS: 2})}))
	require.NoError(t, nodes[0].table.Collect(ctx, 0))

	assert.Len(t, local(t, nodes[0]), 1)
	assert.Zero(t, queued(t, nodes[0]))
	assert.Zero(t, nodes[0].table.GCStats().Collected)
}
