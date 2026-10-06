package table

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// TestLayoutChangeKeepsAckedWrites: a write acknowledged by two of three
// replicas must stay readable after a layout change moves the third slot,
// with one node down — the quorum guarantee, across a layout change.
func TestLayoutChangeKeepsAckedWrites(t *testing.T) {
	ctx := context.Background()
	nodes := clusterOf(t, 4, 3)

	before := nodes[0].member.Layout()
	next, err := layout.Compute(before, roles(4), layout.Options{})
	require.NoError(t, err)

	byID := map[layout.NodeID]*node{}
	for _, n := range nodes {
		byID[n.member.ID()] = n
	}

	// A partition key whose replicas change: one old replica leaves, the
	// new node joins, two stay.
	var pk string

	var gone, stay []layout.NodeID

	for i := range 1000 {
		k := fmt.Sprint("bucket", i)
		old := before.Slots[before.Partition([]byte(k))][:Replicas]
		now := next.Slots[next.Partition([]byte(k))][:Replicas]

		gone, stay = nil, nil

		for _, id := range old {
			if slices.Contains(now, id) {
				stay = append(stay, id)
			} else {
				gone = append(gone, id)
			}
		}

		if len(gone) == 1 && len(stay) == 2 {
			pk = k

			break
		}
	}

	require.NotEmpty(t, pk, "no partition moves one replica")

	// Acknowledged by the replica that leaves and one that stays; the other
	// one that stays missed it.
	w := []wireEntry{row(pk, "k", reg{V: "acked", TS: 1})}
	require.NoError(t, byID[gone[0]].table.localInsert(w))
	require.NoError(t, byID[stay[0]].table.localInsert(w))

	for _, n := range nodes {
		_, err := n.member.Adopt(next)
		require.NoError(t, err)
	}

	// One node down: the replica that stays and holds the write.
	byID[stay[0]].srv.Close()

	got, ok, err := byID[stay[1]].table.Get(ctx, pk, "k")
	require.NoError(t, err)
	require.True(t, ok, "an acknowledged write must not read as missing with one node down")
	require.Equal(t, "acked", got.V)
}
