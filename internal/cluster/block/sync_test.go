package block

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

func TestSyncPullsMissingBlocks(t *testing.T) {
	nodes := cluster(t, 3)
	ctx := context.Background()

	var hashes []Hash

	for i := range 20 {
		h, err := nodes[0].blocks.Put(ctx, fmt.Appendf(nil, "block %d", i))
		require.NoError(t, err)

		hashes = append(hashes, h)
	}

	// n2 lost half of them, and the resync queue that would have noticed is
	// gone, as after a restart.
	for _, h := range hashes[:10] {
		require.NoError(t, nodes[2].store.Delete(h))
	}

	require.NoError(t, nodes[2].blocks.Sync(ctx))

	for _, h := range hashes {
		assert.True(t, nodes[2].store.Has(h))
	}

	assert.Equal(t, int64(10), nodes[2].blocks.SyncStats().Pulled)

	require.NoError(t, nodes[2].blocks.Sync(ctx))
	assert.Zero(t, nodes[2].blocks.SyncStats().OutOfSync, "replicas that agree have nothing to pull")
}

func TestSyncUnreachable(t *testing.T) {
	nodes := cluster(t, 3)
	nodes[2].srv.Close()

	require.NoError(t, nodes[0].blocks.Sync(context.Background()))
	assert.Equal(t, 1, nodes[0].blocks.SyncStats().Unreachable)
}

func TestSyncHandsOver(t *testing.T) {
	nodes := clusterOf(t, 4, 3)
	ctx := context.Background()

	var hashes []Hash

	for i := range 48 {
		h, err := nodes[0].blocks.Put(ctx, fmt.Appendf(nil, "block %d", i))
		require.NoError(t, err)

		hashes = append(hashes, h)
	}

	// Let every first write settle before the layout moves under it.
	require.Eventually(t, func() bool {
		for _, h := range hashes {
			held := 0

			for _, n := range nodes {
				if n.store.Has(h) {
					held++
				}
			}

			if held < Replicas {
				return false
			}
		}

		return true
	}, 5*time.Second, 10*time.Millisecond)

	next, err := layout.Compute(nodes[0].member.Layout(), roles(4), layout.Options{})
	require.NoError(t, err)

	for _, n := range nodes {
		_, err := n.member.Adopt(next)
		require.NoError(t, err)
	}

	for range 2 {
		for _, n := range nodes {
			require.NoError(t, n.blocks.Sync(ctx))
		}
	}

	var handed int64
	for _, n := range nodes {
		handed += n.blocks.SyncStats().HandedOver
	}

	assert.Positive(t, handed)

	for _, h := range hashes {
		owners := next.Slots[next.Partition(h[:])][:Replicas]

		for _, n := range nodes {
			assert.Equal(t, slices.Contains(owners, n.member.ID()), n.store.Has(h),
				"%s on %s (owners %v)", h, n.member.ID(), owners)
		}
	}
}
