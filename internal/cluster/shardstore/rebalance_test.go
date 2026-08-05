package shardstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// spread builds a map of ranges owned as named, and a survey giving each the
// write rate alongside it.
func spread(t *testing.T, owners []cluster.NodeID, rates []float64) (*rangemap.Map, shardstore.Survey) {
	t.Helper()

	if rates != nil {
		require.Len(t, rates, len(owners))
	}

	ranges := make([]rangemap.Range, 0, len(owners))
	survey := make(shardstore.Survey, 0, len(owners))

	for i, owner := range owners {
		start := ""
		if i > 0 {
			start = key(i)
		}

		end := ""
		if i < len(owners)-1 {
			end = key(i + 1)
		}

		ranges = append(ranges, rangemap.Range{Start: start, End: end, Owner: owner})

		rate := 0.0
		if rates != nil {
			rate = rates[i]
		}

		survey = append(survey, &shardstore.Measurement{Writes: rate, Bytes: 1000})
	}

	m := &rangemap.Map{Revision: 3, Ranges: ranges}
	require.NoError(t, m.Validate())

	return m, survey
}

// key is the i-th range boundary, in the object prefix so the map is valid.
func key(i int) string { return string([]byte{'o', byte('a' + i)}) }

// TestRebalanceMovesTheHottestPlaceableRange: the point of the whole phase.
// Load, not bytes — every range here holds the same amount, and the one that
// moves is the one taking the writes.
func TestRebalanceMovesTheHottestPlaceableRange(t *testing.T) {
	// n0 is taking 100/s across three ranges; n1 is idle.
	m, survey := spread(t,
		[]cluster.NodeID{"n0", "n0", "n0", "n1"},
		[]float64{40, 10, 50, 0})

	move, ok := shardstore.PlanRebalance(m, survey,
		[]cluster.NodeID{"n0", "n1"}, shardstore.RebalancePolicy{MinGap: 10})

	require.True(t, ok)
	assert.Equal(t, cluster.NodeID("n1"), move.To, "to the node with capacity to take it")

	// The 50/s range: the hottest that does not overshoot. Exactly half the gap
	// is placeable — moving it leaves the two nodes equal, which is the best a
	// single move can do and the boundary the rule is drawn at.
	assert.Equal(t, key(2), move.At)
	assert.InDelta(t, 50, move.Writes, 0.001)
	assert.InDelta(t, 100, move.Gap, 0.001)
}

// TestRebalanceWillNotOvershoot is the anti-thrash rule, and the reason split
// and move compose.
//
// A range larger than half the gap leaves the destination busier than the source
// was, and the next pass moves it back — a cluster ping-ponging one range
// forever, doing a full copy each way.
func TestRebalanceWillNotOvershoot(t *testing.T) {
	// The gap is 60; the only range on n0 is taking all of it.
	m, survey := spread(t,
		[]cluster.NodeID{"n0", "n1"},
		[]float64{60, 0})

	_, ok := shardstore.PlanRebalance(m, survey,
		[]cluster.NodeID{"n0", "n1"}, shardstore.RebalancePolicy{MinGap: 10})

	assert.False(t, ok, "moving the whole load merely swaps which node is busy")
}

// TestRebalanceMovesOnceTheHotRangeIsSplit is the other half of the same story:
// what a split makes possible.
//
// A node whose load is one enormous range has nothing small enough to place.
// Splitting costs nothing — both halves are already on the right side of the new
// boundary — and produces a piece that fits.
func TestRebalanceMovesOnceTheHotRangeIsSplit(t *testing.T) {
	whole, survey := spread(t,
		[]cluster.NodeID{"n0", "n1"},
		[]float64{60, 0})

	_, ok := shardstore.PlanRebalance(whole, survey,
		[]cluster.NodeID{"n0", "n1"}, shardstore.RebalancePolicy{MinGap: 10})
	require.False(t, ok, "the premise: nothing is placeable yet")

	// The same load, now in two ranges.
	split, halves := spread(t,
		[]cluster.NodeID{"n0", "n0", "n1"},
		[]float64{30, 30, 0})

	move, ok := shardstore.PlanRebalance(split, halves,
		[]cluster.NodeID{"n0", "n1"}, shardstore.RebalancePolicy{MinGap: 10})

	require.True(t, ok, "a half fits where the whole did not")
	assert.Equal(t, cluster.NodeID("n1"), move.To)
	assert.InDelta(t, 30, move.Writes, 0.001)
}

// TestRebalanceLeavesABalancedClusterAlone: below the gap the difference between
// two nodes is noise, sampling, or a burst that will be gone before the copy
// finishes — and a rebalance that chased it would spend the cluster's bandwidth
// on churn.
//
// The case has to be one where a range would otherwise fit, or the overshoot
// rule refuses it anyway and the floor is never consulted. So n0 is barely
// busier than n1 and happens to hold one nearly idle range: without the floor
// that range is copied in full to close a gap of three writes a second.
func TestRebalanceLeavesABalancedClusterAlone(t *testing.T) {
	m, survey := spread(t,
		[]cluster.NodeID{"n0", "n0", "n1"},
		[]float64{10, 1, 8})

	_, ok := shardstore.PlanRebalance(m, survey,
		[]cluster.NodeID{"n0", "n1"}, shardstore.RebalancePolicy{MinGap: 25})

	assert.False(t, ok, "a whole range copied to move three writes a second")
}

// TestRebalanceStartsOnlyOneMove: a move is a full copy. Several at once would
// have the cluster spend its bandwidth on rebalancing rather than serving, and
// each would be decided from a picture of the load the others are changing.
func TestRebalanceStartsOnlyOneMove(t *testing.T) {
	m, survey := spread(t,
		[]cluster.NodeID{"n0", "n0", "n1"},
		[]float64{40, 40, 0})

	started, err := m.StartMove(key(1), "n1")
	require.NoError(t, err)

	_, ok := shardstore.PlanRebalance(started, survey,
		[]cluster.NodeID{"n0", "n1"}, shardstore.RebalancePolicy{MinGap: 10})

	assert.False(t, ok, "a second move planned while one is in flight")
}

// TestRebalanceWillNotPlanAroundAnUnmeasuredRange: a range nobody could measure
// is a range whose owner's load is unknown. Counting it as zero would make an
// unreachable node look idle and elect it as the destination — sending work to
// the one node that could not answer.
func TestRebalanceWillNotPlanAroundAnUnmeasuredRange(t *testing.T) {
	m, survey := spread(t,
		[]cluster.NodeID{"n0", "n0", "n1"},
		[]float64{40, 40, 0})

	survey[2] = nil

	_, ok := shardstore.PlanRebalance(m, survey,
		[]cluster.NodeID{"n0", "n1"}, shardstore.RebalancePolicy{MinGap: 10})

	assert.False(t, ok)
}

// TestRebalanceWillNotPlanAroundAMissingOwner: reassigning a range whose owner
// is gone is the failover path's job, and this pass runs before it has done it.
// Planning around the gap would move a range on behalf of a failure nobody has
// handled.
func TestRebalanceWillNotPlanAroundAMissingOwner(t *testing.T) {
	m, survey := spread(t,
		[]cluster.NodeID{"n0", "n0", "n2"},
		[]float64{40, 40, 0})

	_, ok := shardstore.PlanRebalance(m, survey,
		[]cluster.NodeID{"n0", "n1"}, shardstore.RebalancePolicy{MinGap: 10})

	assert.False(t, ok, "n2 owns a range and is not in the membership")
}

// TestRebalanceGivesWorkToANodeOwningNothing: a node that has just joined is the
// emptiest destination there is, and one that only counted as a destination once
// it appeared in the map would never be given anything at all.
func TestRebalanceGivesWorkToANodeOwningNothing(t *testing.T) {
	m, survey := spread(t,
		[]cluster.NodeID{"n0", "n0"},
		[]float64{40, 40})

	move, ok := shardstore.PlanRebalance(m, survey,
		[]cluster.NodeID{"n0", "fresh"}, shardstore.RebalancePolicy{MinGap: 10})

	require.True(t, ok)
	assert.Equal(t, cluster.NodeID("fresh"), move.To)
}

// TestRebalanceNeedsSomewhereToMoveTo: one node has nowhere to put anything.
func TestRebalanceNeedsSomewhereToMoveTo(t *testing.T) {
	m, survey := spread(t, []cluster.NodeID{"n0", "n0"}, []float64{40, 40})

	_, ok := shardstore.PlanRebalance(m, survey,
		[]cluster.NodeID{"n0"}, shardstore.RebalancePolicy{MinGap: 10})

	assert.False(t, ok)
}

// TestRebalanceIsAFunctionOfTheSurvey: an election has a window where two
// candidates both believe they hold it. Two controllers planning one pass from
// the same survey must pick the same move, so that window produces a duplicate
// write rather than two different moves started at once.
func TestRebalanceIsAFunctionOfTheSurvey(t *testing.T) {
	// Ties everywhere: equal loads on the candidates, equal rates on the ranges.
	m, survey := spread(t,
		[]cluster.NodeID{"n2", "n2", "n1", "n0"},
		[]float64{30, 30, 0, 0})

	live := []cluster.NodeID{"n0", "n1", "n2"}

	first, ok := shardstore.PlanRebalance(m, survey, live, shardstore.RebalancePolicy{MinGap: 10})
	require.True(t, ok)

	for range 20 {
		got, ok := shardstore.PlanRebalance(m, survey, live, shardstore.RebalancePolicy{MinGap: 10})
		require.True(t, ok)
		require.Equal(t, first, got, "the same survey gave two different moves")
	}
}

// rebalanceFixture is a controller over a map with load a test dictates.
func rebalanceFixture(
	t *testing.T,
	m *rangemap.Map,
	rates map[string]float64,
	live ...cluster.NodeID,
) (f *fixture, measured *int) {
	t.Helper()

	calls := 0

	f = &fixture{
		ctl:  &recorder{m: m, state: metastore.StateReady},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: live,
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live: func() []cluster.NodeID { return f.live },
		Measure: func(
			_ context.Context, _ cluster.NodeID, r rangemap.Range,
		) (shardstore.Measurement, error) {
			calls++

			return shardstore.Measurement{Writes: rates[r.Start], Bytes: 10}, nil
		},
		Rebalance: shardstore.RebalancePolicy{MinGap: 10},
		Split:     shardstore.SplitPolicy{MaxBytes: 1 << 30},
		Readiness: f.ctl,
		Now:       f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	return f, &calls
}

// TestControllerStartsAMove is the policy reaching the mechanism: a lopsided
// cluster gets a learner named, which is the first of a move's three edits and
// the only one that decides anything.
func TestControllerStartsAMove(t *testing.T) {
	m, _ := spread(t, []cluster.NodeID{"n0", "n0", "n1"}, nil)

	f, _ := rebalanceFixture(t, m,
		map[string]float64{"": 30, key(1): 30, key(2): 0}, "n0", "n1")

	out := f.reconcile(t)

	assert.True(t, out.Changed)
	require.NotNil(t, out.Started)
	assert.Equal(t, cluster.NodeID("n1"), out.Started.To)

	moving := 0

	for _, r := range f.ctl.m.Ranges {
		if r.MoveTo != "" {
			moving++

			assert.Equal(t, cluster.NodeID("n1"), r.MoveTo)
			assert.Equal(t, []cluster.NodeID{"n1"}, r.Learners,
				"the destination starts as a learner, holding none of the range")
		}
	}

	assert.Equal(t, 1, moving)
}

// TestControllerSurveysOnce: splitting and rebalancing ask the same owners the
// same question. A pass that asked twice would double the traffic on the path
// that runs every five seconds, and could decide two things from two different
// readings of one cluster.
func TestControllerSurveysOnce(t *testing.T) {
	m, _ := spread(t, []cluster.NodeID{"n0", "n0", "n1"}, nil)

	f, measured := rebalanceFixture(t, m,
		map[string]float64{"": 30, key(1): 30, key(2): 0}, "n0", "n1")

	out := f.reconcile(t)
	require.NotNil(t, out.Started, "the premise: this pass both surveyed and moved")

	assert.Equal(t, len(m.Ranges), *measured, "every range was measured twice")
}

// TestControllerDoesNotSplitAndMoveInOnePass: a split rewrites the map, so a
// move planned from the measurements taken before it would be placing ranges
// that no longer exist.
func TestControllerDoesNotSplitAndMoveInOnePass(t *testing.T) {
	m, _ := spread(t, []cluster.NodeID{"n0", "n0", "n1"}, nil)

	f := &fixture{
		ctl:  &recorder{m: m, state: metastore.StateReady},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: []cluster.NodeID{"n0", "n1"},
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live: func() []cluster.NodeID { return f.live },
		Measure: func(
			_ context.Context, _ cluster.NodeID, r rangemap.Range,
		) (shardstore.Measurement, error) {
			// Everything is oversized and n0 is lopsidedly busy: both planners
			// have something to say, and only one of them may.
			return shardstore.Measurement{
				Bytes:   1 << 40,
				SplitAt: r.Start + "m",
				Writes:  map[string]float64{"": 30, key(1): 30, key(2): 0}[r.Start],
			}, nil
		},
		Rebalance: shardstore.RebalancePolicy{MinGap: 10},
		Split:     shardstore.SplitPolicy{MaxBytes: 1000, MaxSplitsPerPass: 4},
		Readiness: f.ctl,
		Now:       f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	out := f.reconcile(t)

	assert.NotEmpty(t, out.Split, "the premise: this pass had splits to make")
	assert.Nil(t, out.Started, "and started a move from measurements it had just invalidated")
}

// TestControllerMoveIsOffWithoutAWayToMeasure: a plane with no transport cannot
// ask an owner anything, and a rebalance on no evidence would move ranges on a
// guess about where the load is.
func TestControllerMoveIsOffWithoutAWayToMeasure(t *testing.T) {
	m, _ := spread(t, []cluster.NodeID{"n0", "n0", "n1"}, nil)

	f := newFixture(t, m, "n0", "n1")

	out := f.reconcile(t)
	assert.False(t, out.Changed)
	assert.Nil(t, out.Started)
}
