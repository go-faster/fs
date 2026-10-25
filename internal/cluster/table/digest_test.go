package table

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

// scanned computes a node's digests from its rows, as the kept ones must be.
func scanned(t *testing.T, n *node) map[Slot][32]byte {
	t.Helper()

	parts := len(n.member.Layout().Slots)
	out := map[Slot][32]byte{}

	require.NoError(t, n.table.scan(func(k, v []byte, pk string) error {
		slot, h := rowSlot(k, pk, parts), rowHash(k, v)

		d := out[slot]
		for i := range d {
			d[i] ^= h[i]
		}

		out[slot] = d

		return nil
	}))

	return out
}

// kept checks every node's kept digests against its rows.
func kept(t *testing.T, nodes []*node) {
	t.Helper()

	for _, n := range nodes {
		got, err := n.table.digests(n.member.Layout(), nil)
		require.NoError(t, err)
		assert.Equal(t, scanned(t, n), got, "%s", n.member.ID())
	}
}

func TestDigestsKept(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)

	// Rows written before the digests are kept are counted when first read.
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "1", reg{V: "a", TS: 1}))
	kept(t, nodes)

	// From then on every change moves them: a write, a merge that changes
	// the row, one that does not, a write only one replica has.
	require.NoError(t, nodes[1].table.Insert(ctx, "b", "2", reg{V: "b", TS: 1}))
	require.NoError(t, nodes[2].table.Insert(ctx, "b", "1", reg{V: "c", TS: 2}))
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "1", reg{V: "old", TS: 1}))
	require.NoError(t, nodes[0].table.localInsert([]wireEntry{row("other", "k", reg{"x", 1})}))
	kept(t, nodes)

	// Compaction replaces one row and deletes another.
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "2", reg{V: "gone", TS: 2}))
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "3", reg{V: "y+tomb", TS: 1}))
	require.NoError(t, nodes[0].table.Collect(ctx, 0))
	kept(t, nodes)

	// A sweep pulls what differs and leaves nothing to find after.
	for _, n := range nodes {
		require.NoError(t, n.table.Sync(ctx))
	}

	kept(t, nodes)

	for _, n := range nodes {
		require.NoError(t, n.table.Sync(ctx))
		assert.Zero(t, n.table.SyncStats().OutOfSync, "%s", n.member.ID())
	}
}

func TestDigestsRebuiltForOtherPartitionCount(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)

	require.NoError(t, nodes[0].table.Insert(ctx, "b", "1", reg{V: "a", TS: 1}))
	kept(t, nodes)

	// Digests stamped for another count would place rows in other slots:
	// they are rebuilt, not read.
	n := nodes[0]
	require.NoError(t, n.table.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(n.table.digestBucket()).Put(digestStamp, binary.BigEndian.AppendUint32(nil, 3))
	}))

	kept(t, nodes)
}
