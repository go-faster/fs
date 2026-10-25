package table

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// regUsage counts a register as one item of its value's length, and a
// collected one as nothing.
func regUsage(r reg) Usage {
	if r.V == "gone" {
		return Usage{}
	}

	return Usage{Items: 1, Bytes: int64(len(r.V))}
}

func counted(t *testing.T, nodes []*node) {
	t.Helper()

	for _, n := range nodes {
		require.NoError(t, n.table.CountUsage(regUsage))
	}
}

// tallied waits for every node's tally of partition "b" to be want.
func tallied(t *testing.T, nodes []*node, want Usage) {
	t.Helper()

	require.Eventually(t, func() bool {
		for _, n := range nodes {
			if got, err := n.table.LocalUsage("b"); err != nil || got != want {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)
}

func TestUsage(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)
	counted(t, nodes)

	require.NoError(t, nodes[0].table.Insert(ctx, "b", "one", reg{V: "abc", TS: 1}))
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "two", reg{V: "de", TS: 1}))
	require.NoError(t, nodes[0].table.Insert(ctx, "other", "k", reg{V: "x", TS: 1}))
	tallied(t, nodes, Usage{Items: 2, Bytes: 5})

	// An overwrite moves the tally by the difference; an older write that
	// merges to nothing new moves it not at all.
	require.NoError(t, nodes[1].table.Insert(ctx, "b", "one", reg{V: "abcdef", TS: 2}))
	require.NoError(t, nodes[2].table.Insert(ctx, "b", "one", reg{V: "zzzzzzzzzz", TS: 1}))
	tallied(t, nodes, Usage{Items: 2, Bytes: 8})

	// A tombstone counts nothing, and collecting it changes nothing.
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "two", reg{V: "gone", TS: 2}))
	tallied(t, nodes, Usage{Items: 1, Bytes: 6})

	require.NoError(t, nodes[0].table.Collect(ctx, 0))
	tallied(t, nodes, Usage{Items: 1, Bytes: 6})

	got, err := nodes[2].table.Usage(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, Usage{Items: 1, Bytes: 6}, got)

	got, err = nodes[2].table.Usage(ctx, "other")
	require.NoError(t, err)
	assert.Equal(t, Usage{Items: 1, Bytes: 1}, got)

	got, err = nodes[2].table.Usage(ctx, "empty")
	require.NoError(t, err)
	assert.Zero(t, got)
}

// TestUsageBackfill checks that a table which held rows before it kept
// tallies starts from their count, not from zero.
func TestUsageBackfill(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)

	require.NoError(t, nodes[0].table.Insert(ctx, "b", "one", reg{V: "abc", TS: 1}))
	require.NoError(t, nodes[0].table.Insert(ctx, "b", "two", reg{V: "gone", TS: 1}))
	require.NoError(t, nodes[0].table.Insert(ctx, "other", "k", reg{V: "x", TS: 1}))

	// Wait for every replica to hold the rows before it starts counting.
	require.Eventually(t, func() bool {
		for _, n := range nodes {
			if r, err := n.table.localGet("other", "k"); err != nil || !r.Found {
				return false
			}
		}

		return true
	}, 5*time.Second, time.Millisecond)

	counted(t, nodes)
	tallied(t, nodes, Usage{Items: 1, Bytes: 3})

	// Deleting a backfilled row brings the tally to zero, not below.
	require.NoError(t, nodes[1].table.Insert(ctx, "b", "one", reg{V: "gone", TS: 2}))
	tallied(t, nodes, Usage{})
}

func TestUsageQuorumIsLargest(t *testing.T) {
	ctx := context.Background()
	nodes := cluster(t)
	counted(t, nodes)

	// One replica has a write the others missed.
	require.NoError(t, nodes[2].table.localInsert([]wireEntry{row("b", "k", reg{V: "abc", TS: 1})}))

	for range 10 {
		got, err := nodes[0].table.Usage(ctx, "b")
		require.NoError(t, err)
		assert.Contains(t, []Usage{{}, {Items: 1, Bytes: 3}}, got)
	}

	require.NoError(t, nodes[0].table.Sync(ctx))
	require.NoError(t, nodes[1].table.Sync(ctx))

	got, err := nodes[0].table.Usage(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, Usage{Items: 1, Bytes: 3}, got)
}

func TestUsageHandOverDrops(t *testing.T) {
	nodes := cluster(t)
	counted(t, nodes)

	tbl := nodes[0].table
	e := row("b", "k", reg{V: "abc", TS: 1})
	require.NoError(t, tbl.localInsert([]wireEntry{e}))

	n, err := tbl.dropIfUnchanged([]wireEntry{e})
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := tbl.LocalUsage("b")
	require.NoError(t, err)
	assert.Zero(t, got)
}
