package block

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
)

func alive(context.Context, Hash) (bool, error) { return true, nil }

// putEverywhere writes a coded block and waits for all its shards.
func putEverywhere(t *testing.T, nodes []*node, data []byte, s Scheme) (Hash, []layout.NodeID) {
	t.Helper()

	h := Sum(data)
	require.NoError(t, nodes[0].blocks.PutCoded(context.Background(), h, data, s, nil))

	slots, err := nodes[0].blocks.slotsFor(h, s)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		for i, id := range slots {
			if !byID(nodes, id).store.HasShard(Shard{Hash: h, K: s.K, M: s.M, I: i}) {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)

	return h, slots
}

func TestRepairRebuildsMissingShard(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	s := Scheme{K: 4, M: 2}
	data := bytes.Repeat([]byte("repair me "), 50000)
	h, slots := putEverywhere(t, nodes, data, s)

	// Shard 1 is lost, and shard 4's node lost its copy too.
	for _, i := range []int{1, 4} {
		require.NoError(t, byID(nodes, slots[i]).store.DeleteShard(Shard{Hash: h, K: 4, M: 2, I: i}))
	}

	first := byID(nodes, slots[0])
	require.NoError(t, first.blocks.RepairShards(ctx, alive))
	assert.Equal(t, 1, first.blocks.ShardStats().Critical, "the first slot counts four of six: one loss from K")

	for _, i := range []int{1, 4} {
		n := byID(nodes, slots[i])
		require.NoError(t, n.blocks.RepairShards(ctx, alive))
		assert.Equal(t, int64(1), n.blocks.ShardStats().Rebuilt)
	}

	require.NoError(t, first.blocks.RepairShards(ctx, alive))
	assert.Zero(t, first.blocks.ShardStats().Critical)
	assert.Zero(t, first.blocks.ShardStats().Degraded, "back to full redundancy")

	// The rebuilt shards are the right bytes: a read using them decodes
	// nothing and the block comes back whole.
	for _, i := range []int{0, 2} {
		require.NoError(t, byID(nodes, slots[i]).store.DeleteShard(Shard{Hash: h, K: 4, M: 2, I: i}))
	}

	got, err := first.blocks.GetCoded(ctx, h, len(data), s)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestRepairSkipsDeadBlocks(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	s := Scheme{K: 2, M: 1}
	h, slots := putEverywhere(t, nodes, []byte("garbage"), s)

	require.NoError(t, byID(nodes, slots[2]).store.DeleteShard(Shard{Hash: h, K: 2, M: 1, I: 2}))

	dead := func(context.Context, Hash) (bool, error) { return false, nil }
	n := byID(nodes, slots[2])
	require.NoError(t, n.blocks.RepairShards(ctx, dead))
	assert.False(t, n.store.HasShard(Shard{Hash: h, K: 2, M: 1, I: 2}), "an unreferenced block is left to GC")
}

func TestRepairCountsLost(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 6, 6, 3, 6)
	s := Scheme{K: 4, M: 2}
	h, slots := putEverywhere(t, nodes, bytes.Repeat([]byte("x"), 4096), s)

	for _, i := range []int{1, 2, 3} {
		require.NoError(t, byID(nodes, slots[i]).store.DeleteShard(Shard{Hash: h, K: 4, M: 2, I: i}))
	}

	first := byID(nodes, slots[0])
	require.NoError(t, first.blocks.RepairShards(ctx, alive))
	assert.Equal(t, 1, first.blocks.ShardStats().Lost)
}

// TestShardHandOver: a layout change that moves a slot moves its shard, and
// the block reads whole from the new owners.
func TestShardHandOver(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 7, 6, 3, 6)
	s := Scheme{K: 4, M: 2}

	type written struct {
		data   []byte
		h      Hash
		before []layout.NodeID
	}

	var blocks []written

	for i := range 16 {
		data := bytes.Repeat([]byte{byte(i)}, 20000+i)
		h, before := putEverywhere(t, nodes, data, s)
		blocks = append(blocks, written{data, h, before})
	}

	next, err := layout.Compute(nodes[0].member.Layout(), roles(7), layout.Options{Partitions: 8, Widths: []int{3, 6}})
	require.NoError(t, err)

	for _, n := range nodes {
		_, err := n.member.Adopt(next)
		require.NoError(t, err)
	}

	for _, n := range nodes {
		require.NoError(t, n.blocks.RepairShards(ctx, alive))
	}

	moved := 0

	for _, b := range blocks {
		slots, err := nodes[0].blocks.slotsFor(b.h, s)
		require.NoError(t, err)

		if !slices.Equal(b.before, slots) {
			moved++
		}

		for i, id := range slots {
			assert.True(t, byID(nodes, id).store.HasShard(Shard{Hash: b.h, K: 4, M: 2, I: i}), "shard %d on its slot", i)
		}

		got, err := nodes[6].blocks.GetCoded(ctx, b.h, len(b.data), s)
		require.NoError(t, err)
		assert.Equal(t, b.data, got)
	}

	require.Positive(t, moved, "the new node takes slots, or the test proves nothing")
	assert.Zero(t, nodes[6].blocks.Stats().Degraded, "every shard reached its new slot")

	// The old holders keep their copies while the old version is retained;
	// once every node has synced the new one, it retires and they go.
	for _, n := range nodes {
		require.NoError(t, n.member.MarkSynced(next.Version))
	}

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
		require.NoError(t, n.blocks.RepairShards(ctx, alive))
	}

	var shards int

	for _, n := range nodes {
		require.NoError(t, n.store.WalkShards(func(Shard, time.Time) error { shards++; return nil }))
	}

	assert.Equal(t, 6*len(blocks), shards, "and was dropped where it was")
}
