package table

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// local returns every row a node stores, by "pk/sk".
func local(t *testing.T, n *node) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, n.table.scan(func(k, v []byte, pk string) error {
		out[pk+"/"+string(k[len(key(pk, "")):])] = string(v)

		return nil
	}))

	return out
}

func row(pk, sk string, r reg) wireEntry {
	return wireEntry{PK: pk, SK: sk, Row: encodeReg(r)}
}

func encodeReg(r reg) []byte { return fmt.Appendf(nil, `{"v":%q,"ts":%d}`, r.V, r.TS) }

func TestSyncPullsMissedWrites(t *testing.T) {
	nodes := cluster(t)
	ctx := context.Background()

	// n2 missed two writes, and n0 and n1 each missed one of the others'.
	require.NoError(t, nodes[0].table.localInsert([]wireEntry{row("b", "1", reg{"a", 1}), row("b", "2", reg{"x", 1})}))
	require.NoError(t, nodes[1].table.localInsert([]wireEntry{row("b", "1", reg{"a", 1}), row("b", "2", reg{"y", 2})}))

	for _, n := range nodes {
		require.NoError(t, n.table.Sync(ctx))
	}

	want := local(t, nodes[0])
	assert.Len(t, want, 2)
	assert.JSONEq(t, `{"v":"y","ts":2}`, want["b/2"], "divergent copies converge on the merge")

	for _, n := range nodes[1:] {
		assert.Equal(t, want, local(t, n), "%s", n.member.ID())
	}

	assert.Positive(t, nodes[2].table.SyncStats().Pulled)

	// A sweep over replicas that agree finds nothing to pull.
	for _, n := range nodes {
		require.NoError(t, n.table.Sync(ctx))
		assert.Zero(t, n.table.SyncStats().OutOfSync)
	}
}

func TestSyncPages(t *testing.T) {
	nodes := cluster(t)

	var rows []wireEntry
	for i := range syncPage*2 + 17 {
		rows = append(rows, row("bucket", fmt.Sprintf("k%05d", i), reg{"v", 1}))
	}

	require.NoError(t, nodes[0].table.localInsert(rows))
	require.NoError(t, nodes[1].table.Sync(context.Background()))

	assert.Len(t, local(t, nodes[1]), len(rows))
}

func TestSyncUnreachable(t *testing.T) {
	nodes := cluster(t)
	nodes[2].srv.Close()

	require.ErrorIs(t, nodes[0].table.Sync(context.Background()), ErrIncomplete,
		"an unreachable replica leaves the sweep incomplete: the node has not synced")
	assert.Equal(t, 1, nodes[0].table.SyncStats().Unreachable)
	assert.False(t, nodes[0].table.SyncStats().LastSweep.IsZero())
}

func TestSyncHandsOver(t *testing.T) {
	// Three of four nodes hold data; then the fourth joins the layout and
	// takes some partitions over.
	nodes := clusterOf(t, 4, 3)
	ctx := context.Background()

	for i := range 64 {
		require.NoError(t, nodes[0].table.Insert(ctx, fmt.Sprint("bucket", i), "key", reg{"v", 1}))
	}

	before := nodes[0].member.Layout()
	next, err := layout.Compute(before, roles(4), layout.Options{})
	require.NoError(t, err)

	for _, n := range nodes {
		_, err := n.member.Adopt(next)
		require.NoError(t, err)
	}

	// New owners pull, old ones copy theirs over and keep them: the old
	// version is retained until every node has synced the new one.
	for range 2 {
		for _, n := range nodes {
			require.NoError(t, n.table.Sync(ctx))
		}
	}

	require.Len(t, nodes[0].member.Layouts(), 2, "the old version is retained")

	for _, n := range nodes {
		require.NoError(t, n.member.MarkSynced(next.Version))
	}

	// Gossip carries every node's sync; once all have it, the old version
	// retires and the next sweep drops what moved.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			n.member.Round(ctx)
		}

		for _, n := range nodes {
			if len(n.member.Layouts()) != 1 {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)

	for _, n := range nodes {
		require.NoError(t, n.table.Sync(ctx))
	}

	var handed int64
	for _, n := range nodes {
		handed += n.table.SyncStats().HandedOver
	}

	assert.Positive(t, handed)

	for i := range 64 {
		pk := fmt.Sprint("bucket", i)
		owners := next.Slots[next.Partition([]byte(pk))][:Replicas]

		for _, n := range nodes {
			_, held := local(t, n)[pk+"/key"]
			assert.Equal(t, slices.Contains(owners, n.member.ID()), held,
				"%s on %s (owners %v)", pk, n.member.ID(), owners)
		}

		got, ok, err := nodes[3].table.Get(ctx, pk, "key")
		require.NoError(t, err)
		require.True(t, ok, "%s still reads after the move", pk)
		assert.Equal(t, "v", got.V)
	}
}
