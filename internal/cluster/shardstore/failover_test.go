package shardstore_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// mapWith builds a partition from (owner, followers...) triples in key order.
func mapWith(specs ...[]cluster.NodeID) *rangemap.Map {
	bounds := []string{"", "oc", "of", "oi", "ol", "oo", "or", "ou"}

	m := &rangemap.Map{Revision: 5}

	for i, spec := range specs {
		r := rangemap.Range{Start: bounds[i], Owner: spec[0]}
		if len(spec) > 1 {
			r.Followers = spec[1:]
		}

		if i < len(specs)-1 {
			r.End = bounds[i+1]
		}

		m.Ranges = append(m.Ranges, r)
	}

	return m
}

// TestHealthyRangesAreLeftAlone: this is failover, not rebalancing. Moving a
// range whose owner is fine would cost a data move for no reason.
func TestHealthyRangesAreLeftAlone(t *testing.T) {
	m := mapWith([]cluster.NodeID{"n0", "n1"}, []cluster.NodeID{"n1", "n2"})

	got, err := shardstore.Reassign(m, []cluster.NodeID{"n0", "n1", "n2"})
	require.NoError(t, err)

	assert.Equal(t, m.Ranges, got.Map.Ranges)
	assert.Empty(t, got.Promoted)
	assert.Empty(t, got.Orphaned)
}

// TestPromotionIsTheCheapCase: the follower already holds what the owner held,
// so this is a metadata change and nothing moves.
func TestPromotionIsTheCheapCase(t *testing.T) {
	m := mapWith(
		[]cluster.NodeID{"n0", "n1", "n2"},
		[]cluster.NodeID{"n1", "n0"},
	)

	got, err := shardstore.Reassign(m, []cluster.NodeID{"n1", "n2"})
	require.NoError(t, err)

	require.Len(t, got.Promoted, 1)
	assert.EqualValues(t, "n1", got.Map.Ranges[0].Owner, "the first live follower took over")
	assert.Empty(t, got.Orphaned)

	// The promoted node is no longer one of its own followers. Restoring R is
	// re-replication, which is a data move and deliberately not decided here.
	assert.Equal(t, []cluster.NodeID{"n2"}, got.Map.Ranges[0].Followers)
}

// TestOrphanedWhenNoFollowerSurvives is the expensive case, and the one a
// caller has to act on. The range is still assigned — a range with no owner is
// a key nothing serves, which is worse — but the node holds nothing for it.
func TestOrphanedWhenNoFollowerSurvives(t *testing.T) {
	m := mapWith(
		[]cluster.NodeID{"n0", "n3"}, // both gone
		[]cluster.NodeID{"n1"},
	)

	got, err := shardstore.Reassign(m, []cluster.NodeID{"n1", "n2"})
	require.NoError(t, err)

	require.Len(t, got.Orphaned, 1)
	assert.Empty(t, got.Promoted)

	// Assigned anyway, and the map is still a partition.
	require.NoError(t, got.Map.Validate())
	assert.Contains(t, []cluster.NodeID{"n1", "n2"}, got.Map.Ranges[0].Owner)
}

// TestOrphansSpreadRatherThanPile: load is counted as the decision proceeds, so
// several orphans do not all land on whichever node happened to be least loaded
// at the start — which would replace one dead node with one overloaded one.
func TestOrphansSpreadRatherThanPile(t *testing.T) {
	m := mapWith(
		[]cluster.NodeID{"gone"},
		[]cluster.NodeID{"gone"},
		[]cluster.NodeID{"gone"},
		[]cluster.NodeID{"gone"},
	)

	got, err := shardstore.Reassign(m, []cluster.NodeID{"n0", "n1"})
	require.NoError(t, err)

	require.Len(t, got.Orphaned, 4)

	counts := map[cluster.NodeID]int{}
	for _, r := range got.Map.Ranges {
		counts[r.Owner]++
	}

	assert.Equal(t, 2, counts["n0"])
	assert.Equal(t, 2, counts["n1"])
}

// TestReassignmentIsDeterministic: two nodes racing to reassign the same
// failure must produce the same map. If they did not, the fenced write that
// decides between them would be settling a disagreement rather than a
// duplicate — and the loser's view of who owns what would be wrong.
func TestReassignmentIsDeterministic(t *testing.T) {
	m := mapWith(
		[]cluster.NodeID{"gone"},
		[]cluster.NodeID{"gone", "n2"},
		[]cluster.NodeID{"gone"},
	)

	first, err := shardstore.Reassign(m, []cluster.NodeID{"n2", "n0", "n1"})
	require.NoError(t, err)

	// The same inputs in a different order, as another node would see them.
	second, err := shardstore.Reassign(m, []cluster.NodeID{"n1", "n0", "n2"})
	require.NoError(t, err)

	assert.Equal(t, first.Map.Ranges, second.Map.Ranges)
}

// TestReassignRefusesWithoutSomewhereToGo: producing a map with a dead owner,
// or no map at all, would be worse than refusing — the first is a partition
// nothing serves and the second is not a partition.
func TestReassignRefusesWithoutSomewhereToGo(t *testing.T) {
	m := mapWith([]cluster.NodeID{"n0"})

	_, err := shardstore.Reassign(m, nil)
	require.ErrorContains(t, err, "no live nodes")

	_, err = shardstore.Reassign(nil, []cluster.NodeID{"n0"})
	require.ErrorContains(t, err, "no range map")
}

// TestReassignRefusesAnInvalidMap: a gap in equals a gap out, and a gap is a
// key nothing owns. Better to refuse than to propagate one into the map that
// gets written.
func TestReassignRefusesAnInvalidMap(t *testing.T) {
	broken := &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "om", Owner: "n0"},
		{Start: "op", End: "", Owner: "n1"},
	}}

	_, err := shardstore.Reassign(broken, []cluster.NodeID{"n0", "n1"})
	require.ErrorContains(t, err, "gap or overlap")
}

// TestPromotionPrefersEarlierFollowers pins what the ordering means today.
//
// Currency is what should decide this, and nothing here knows how far behind
// each follower is — so order is the only signal available. Promoting a stale
// follower costs the plane the entries it missed, and nothing repairs those
// short of a rebuild, which is why lag is the input this wants as soon as it
// exists.
func TestPromotionPrefersEarlierFollowers(t *testing.T) {
	m := mapWith([]cluster.NodeID{"gone", "n1", "n2"})

	got, err := shardstore.Reassign(m, []cluster.NodeID{"n1", "n2"})
	require.NoError(t, err)

	assert.EqualValues(t, "n1", got.Map.Ranges[0].Owner)

	// And a follower that is itself gone is skipped rather than promoted.
	m = mapWith([]cluster.NodeID{"gone", "alsogone", "n2"})

	got, err = shardstore.Reassign(m, []cluster.NodeID{"n2"})
	require.NoError(t, err)

	assert.EqualValues(t, "n2", got.Map.Ranges[0].Owner)
	require.Len(t, got.Promoted, 1, "a live follower further down the list is still a promotion")
}

// TestALearnerIsNeverPromoted is the safety property the whole learner concept
// exists for, and the reason a move can be built on top of failover at all.
//
// A learner is mid-backfill: it holds *some* of the range. Promoting it answers
// "no such object" for every key the backfill has not reached — and nothing
// reports that, because a partial range is a range that simply says no. So it
// must be less eligible than an empty node, which at least gets rebuilt.
func TestALearnerIsNeverPromoted(t *testing.T) {
	m := &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "", Owner: "n0", Learners: []cluster.NodeID{"n1"}},
	}}
	require.NoError(t, m.Validate())

	out, err := shardstore.Reassign(m, []cluster.NodeID{"n1", "n2"})
	require.NoError(t, err)

	require.Empty(t, out.Promoted, "a learner is not a promotion candidate")
	require.Len(t, out.Orphaned, 1,
		"with only a learner available this is the expensive case, and saying so is the point")
}

// TestAFollowerBeatsALearner: given both, the follower wins — it is current,
// and the learner is not yet.
func TestAFollowerBeatsALearner(t *testing.T) {
	m := &rangemap.Map{Ranges: []rangemap.Range{{
		Start: "", End: "", Owner: "n0",
		Followers: []cluster.NodeID{"n2"},
		Learners:  []cluster.NodeID{"n1"},
	}}}
	require.NoError(t, m.Validate())

	out, err := shardstore.Reassign(m, []cluster.NodeID{"n1", "n2"})
	require.NoError(t, err)

	require.Len(t, out.Promoted, 1)
	assert.Equal(t, cluster.NodeID("n2"), out.Map.Ranges[0].Owner)
	assert.Equal(t, []cluster.NodeID{"n1"}, out.Map.Ranges[0].Learners,
		"the move in flight is not canceled by a failover it was not part of")
}

// TestAPromotedNodeStopsLearning: a node that becomes the owner cannot also be
// listed as learning from itself, which Validate refuses — so the reassignment
// has to take it out of both lists.
func TestAPromotedNodeStopsLearning(t *testing.T) {
	m := &rangemap.Map{Ranges: []rangemap.Range{{
		Start: "", End: "", Owner: "n0",
		Learners: []cluster.NodeID{"n1"},
	}}}
	require.NoError(t, m.Validate())

	// Only the learner is alive, so it takes the range as an orphan.
	out, err := shardstore.Reassign(m, []cluster.NodeID{"n1"})
	require.NoError(t, err)
	require.NoError(t, out.Map.Validate())

	assert.Equal(t, cluster.NodeID("n1"), out.Map.Ranges[0].Owner)
	assert.Empty(t, out.Map.Ranges[0].Learners)
	assert.Len(t, out.Orphaned, 1, "what it holds is still partial, which is what orphaned means")
}
