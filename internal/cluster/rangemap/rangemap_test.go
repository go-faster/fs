package rangemap_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// three is a hand-built map, so the lookup tests do not depend on whatever
// boundaries Initial happens to choose.
func three() *rangemap.Map {
	return &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "om", Owner: "n0"},
		{Start: "om", End: "ot", Owner: "n1"},
		{Start: "ot", End: "", Owner: "n2"},
	}}
}

// TestLookupIsTotal: the ranges cover the key space, so every key has an owner.
// This is the property a gap would break, and it would break it silently —
// routing the key to the range before it, whose owner does not hold it, so the
// object reads as absent rather than as an error.
func TestLookupIsTotal(t *testing.T) {
	m := three()

	for _, key := range []string{
		"",         // the very start
		"\x00",     // before any object key
		"oa", "ol", // first range
		"om",        // exactly a boundary: belongs to the range it starts
		"omm", "os", // second range
		"ot",                // the next boundary
		"ozzz",              // last range
		"p", "\xff\xff\xff", // past every object key, still owned
	} {
		t.Run(fmt.Sprintf("%q", key), func(t *testing.T) {
			r, ok := m.Lookup(key)
			require.True(t, ok, "every key must have an owner")
			assert.True(t, r.Contains(key), "the range returned must actually contain the key")
		})
	}
}

// TestLookupBoundariesAreHalfOpen: a boundary key belongs to the range it
// starts, not the one it ends. Getting this wrong puts one key on the wrong
// node — invisible until that exact key is written.
func TestLookupBoundariesAreHalfOpen(t *testing.T) {
	m := three()

	owner, ok := m.Owner("om")
	require.True(t, ok)
	assert.EqualValues(t, "n1", owner, "a boundary starts its range")

	owner, ok = m.Owner("ol\xff")
	require.True(t, ok)
	assert.EqualValues(t, "n0", owner, "the key just below belongs to the previous range")
}

// TestLookupOnEmptyMap distinguishes "not initialized" from "unowned key". The
// second cannot happen in a valid map; the first is a cluster whose metadata
// plane has not been set up, and a caller should be able to tell.
func TestLookupOnEmptyMap(t *testing.T) {
	var m rangemap.Map

	_, ok := m.Lookup("anything")
	assert.False(t, ok)
}

func TestRangesFor(t *testing.T) {
	m := three()

	assert.Equal(t, []rangemap.Range{{Start: "om", End: "ot", Owner: "n1"}}, m.RangesFor("n1"))
	assert.Empty(t, m.RangesFor("n9"), "a node owning nothing owns nothing")
}

// TestValidateRejectsWhatWouldFailSilently is the point of Validate: none of
// these produce an error at lookup time, they produce a wrong answer.
func TestValidateRejectsWhatWouldFailSilently(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges []rangemap.Range
		want   string
	}{
		{
			name:   "empty",
			ranges: nil,
			want:   "empty",
		},
		{
			name: "a gap between ranges",
			ranges: []rangemap.Range{
				{Start: "", End: "om", Owner: "n0"},
				{Start: "op", End: "", Owner: "n1"},
			},
			want: "gap or overlap",
		},
		{
			name: "an overlap between ranges",
			ranges: []rangemap.Range{
				{Start: "", End: "ot", Owner: "n0"},
				{Start: "om", End: "", Owner: "n1"},
			},
			want: "gap or overlap",
		},
		{
			name: "not starting at the beginning",
			ranges: []rangemap.Range{
				{Start: "oa", End: "", Owner: "n0"},
			},
			want: "not the empty key",
		},
		{
			name: "not reaching the end",
			ranges: []rangemap.Range{
				{Start: "", End: "om", Owner: "n0"},
			},
			want: "not the end of the key space",
		},
		{
			name: "an unowned range",
			ranges: []rangemap.Range{
				{Start: "", End: "", Owner: ""},
			},
			want: "no owner",
		},
		{
			name: "an inverted range",
			ranges: []rangemap.Range{
				{Start: "", End: "ot", Owner: "n0"},
				{Start: "ot", End: "om", Owner: "n1"},
			},
			want: "empty or inverted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &rangemap.Map{Ranges: tc.ranges}
			require.ErrorContains(t, m.Validate(), tc.want)
		})
	}
}

func TestValidateAcceptsAWholeMap(t *testing.T) {
	require.NoError(t, three().Validate())
}

// unracked turns bare IDs into nodes with no rack, which makes every node its
// own failure domain.
func unracked(ids ...cluster.NodeID) []cluster.Node {
	out := make([]cluster.Node, 0, len(ids))
	for _, id := range ids {
		out = append(out, cluster.Node{ID: id})
	}

	return out
}

// racked builds nodes labeled with the racks given, one per node.
func racked(racks ...string) []cluster.Node {
	out := make([]cluster.Node, 0, len(racks))
	for i, rack := range racks {
		out = append(out, cluster.Node{ID: cluster.NodeID(fmt.Sprintf("n%d", i)), Rack: rack})
	}

	return out
}

// TestInitialPartitionsTheKeySpace: whatever the boundaries, the result has to
// be a partition — that is the invariant, and the boundaries are a guess.
func TestInitialPartitionsTheKeySpace(t *testing.T) {
	nodes := unracked("n0", "n1", "n2")

	for _, n := range []int{1, 2, 3, 7, 16, 64} {
		t.Run(fmt.Sprintf("%d ranges", n), func(t *testing.T) {
			m, err := rangemap.Initial(n, nodes, 1)
			require.NoError(t, err)
			require.NoError(t, m.Validate())
			require.Len(t, m.Ranges, n)

			// Every key lands somewhere, including ones outside the band the
			// boundaries were chosen from.
			for _, key := range []string{"", "o", "oa", "obucket\x00key", "oz", "p", "\xff"} {
				_, ok := m.Lookup(key)
				assert.True(t, ok, "key %q must be owned", key)
			}
		})
	}
}

// TestInitialSpreadsAcrossNodes: a presplit that gave every range to one node
// would be a partition and still pointless.
func TestInitialSpreadsAcrossNodes(t *testing.T) {
	nodes := unracked("n0", "n1", "n2")

	m, err := rangemap.Initial(9, nodes, 1)
	require.NoError(t, err)

	for _, node := range nodes {
		assert.Len(t, m.RangesFor(node.ID), 3, "node %s", node.ID)
	}
}

func TestInitialRefusesNonsense(t *testing.T) {
	_, err := rangemap.Initial(0, unracked("n0"), 1)
	require.ErrorContains(t, err, "at least 1")

	_, err = rangemap.Initial(4, nil, 1)
	require.ErrorContains(t, err, "no nodes")

	_, err = rangemap.Initial(4, unracked("n0"), 0)
	require.ErrorContains(t, err, "at least 1",
		"zero replicas is a range nobody owns, not an unreplicated one")
}

// TestInitialBoundariesAreOrdered guards the arithmetic in boundary(): a
// rounding mistake there produces duplicate or descending split points, which
// Validate catches as an inverted range — but only if someone runs it.
func TestInitialBoundariesAreOrdered(t *testing.T) {
	m, err := rangemap.Initial(32, unracked("n0"), 1)
	require.NoError(t, err)

	for i := 1; i < len(m.Ranges); i++ {
		assert.Less(t, m.Ranges[i-1].Start, m.Ranges[i].Start,
			"boundary %d must sort after its predecessor", i)
	}
}

// TestFollowersMakeALostOwnerCheap is why followers are placed at all.
//
// A range with no follower costs a cluster-wide walk of every disk when its
// owner is lost — the new owner starts empty, and nothing short of rebuilding
// makes it right. A range with one costs a metadata write. So a fresh plane
// asked for replicas gets them on every range, not on average.
func TestFollowersMakeALostOwnerCheap(t *testing.T) {
	m, err := rangemap.Initial(12, unracked("n0", "n1", "n2", "n3"), 3)
	require.NoError(t, err)

	for i, r := range m.Ranges {
		assert.Len(t, r.Followers, 2, "range %d", i)
		assert.NotContains(t, r.Followers, r.Owner,
			"range %d follows itself, which replicates nothing", i)
		assert.Len(t, slices.Compact(slices.Clone(r.Followers)), len(r.Followers),
			"range %d has a duplicate follower, which is one replica pretending to be two", i)
	}
}

// TestFollowersPreferAnotherRack: a follower in the owner's rack survives the
// owner and not the rack, which is the failure the label exists to describe.
func TestFollowersPreferAnotherRack(t *testing.T) {
	// Six nodes, two racks, three each. Every follower should be across.
	nodes := racked("a", "a", "a", "b", "b", "b")

	m, err := rangemap.Initial(6, nodes, 2)
	require.NoError(t, err)

	rackOf := make(map[cluster.NodeID]string, len(nodes))
	for _, n := range nodes {
		rackOf[n.ID] = n.Rack
	}

	for i, r := range m.Ranges {
		require.Len(t, r.Followers, 1)
		assert.NotEqual(t, rackOf[r.Owner], rackOf[r.Followers[0]],
			"range %d keeps its replica in the owner's rack", i)
	}
}

// TestASingleRackStillGetsFollowers: a cluster too small to spread degrades to
// same-rack replicas rather than to none.
//
// A follower sharing a rack still covers every single-node failure, which is
// most of them. Refusing to place one would trade the common case for the rare
// one.
func TestASingleRackStillGetsFollowers(t *testing.T) {
	m, err := rangemap.Initial(4, racked("only", "only", "only"), 3)
	require.NoError(t, err)

	for i, r := range m.Ranges {
		assert.Len(t, r.Followers, 2, "range %d", i)
	}
}

// TestUnlabeledNodesDoNotShareAFate: a node with no rack is its own failure
// domain, per cluster.Node.
//
// Asserted on a mixed cluster, because it is the only place the difference
// shows. If unlabeled nodes shared one domain, the first pass would skip every
// one of them and the follower would be whichever node happens to carry a rack
// label — the opposite of spreading. With them distinct, the nearest node in
// the ring wins, label or not.
func TestUnlabeledNodesDoNotShareAFate(t *testing.T) {
	nodes := []cluster.Node{{ID: "n0"}, {ID: "n1"}, {ID: "n2", Rack: "a"}}

	m, err := rangemap.Initial(1, nodes, 2)
	require.NoError(t, err)

	require.Len(t, m.Ranges, 1)
	assert.Equal(t, []cluster.NodeID{"n1"}, m.Ranges[0].Followers,
		"n1 shares no fate with n0, so it is taken before the labeled node further round")

	// And with every node unlabeled, all of them are available to spread over.
	all, err := rangemap.Initial(3, unracked("n0", "n1", "n2"), 3)
	require.NoError(t, err)

	for i, r := range all.Ranges {
		assert.Len(t, r.Followers, 2, "range %d", i)
	}
}

// TestFollowersAreCappedByTheCluster: asking for more replicas than there are
// nodes gets every other node and no duplicates.
//
// Short rather than padded: a follower list naming a node twice is one replica
// pretending to be two, and a failover would promote into a range no more
// current than the one it left.
func TestFollowersAreCappedByTheCluster(t *testing.T) {
	m, err := rangemap.Initial(2, unracked("n0", "n1"), 5)
	require.NoError(t, err)

	for i, r := range m.Ranges {
		assert.Len(t, r.Followers, 1, "range %d: only one other node exists", i)
	}
}

// TestReplicationSpreadsTheFollowerLoad: every node should carry roughly the
// same number of ranges as a follower, or the promotion after a failure lands
// entirely on one machine.
func TestReplicationSpreadsTheFollowerLoad(t *testing.T) {
	nodes := unracked("n0", "n1", "n2", "n3", "n4", "n5")

	m, err := rangemap.Initial(24, nodes, 3)
	require.NoError(t, err)

	load := make(map[cluster.NodeID]int, len(nodes))

	for _, r := range m.Ranges {
		for _, f := range r.Followers {
			load[f]++
		}
	}

	// 24 ranges × 2 followers = 48 assignments over 6 nodes: 8 each if perfect.
	for _, n := range nodes {
		assert.InDelta(t, 8, load[n.ID], 2, "node %s carries a lopsided share", n.ID)
	}
}
