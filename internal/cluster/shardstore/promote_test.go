package shardstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// readiness is what learners say when asked, plus which of them were asked.
//
// The asking is worth recording on its own: a controller that promoted without
// asking, or that asked the owner instead of the learner, would pass a test that
// only looked at the map it wrote.
type readiness struct {
	ready map[cluster.NodeID]bool
	fail  map[cluster.NodeID]bool
	asked []cluster.NodeID
}

func (r *readiness) answer(_ context.Context, node cluster.NodeID, _ rangemap.Range) (bool, error) {
	r.asked = append(r.asked, node)

	if r.fail[node] {
		return false, errors.Errorf("node %s is unreachable", node)
	}

	return r.ready[node], nil
}

// movingFixture is a controller over a map with a move in flight: n0 owns, n1
// follows, n2 is being copied into.
func movingFixture(t *testing.T, r *readiness, live ...cluster.NodeID) *fixture {
	t.Helper()

	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{{
		Start: "", End: "", Owner: "n0",
		Followers: []cluster.NodeID{"n1"},
		Learners:  []cluster.NodeID{"n2"},
	}}}

	require.NoError(t, m.Validate())

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: live,
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load:         f.ctl.load,
		Save:         f.ctl.save,
		Live:         func() []cluster.NodeID { return f.live },
		Readiness:    f.ctl,
		Ready:        r.answer,
		PromoteAfter: 30 * time.Second,
		RebuildAfter: 10 * time.Minute,
		Now:          f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	return f
}

// TestCaughtUpLearnerBecomesAFollower is the move's last step: the copy is done,
// so the node that was being copied into stops being un-promotable.
func TestCaughtUpLearnerBecomesAFollower(t *testing.T) {
	r := &readiness{ready: map[cluster.NodeID]bool{"n2": true}}
	f := movingFixture(t, r, "n0", "n1", "n2")

	out := f.reconcile(t)

	assert.True(t, out.Changed)
	require.Len(t, out.Learned, 1)

	assert.Equal(t, []cluster.NodeID{"n2"}, r.asked,
		"the learner is the only node that knows what landed")

	got := f.ctl.m.Ranges[0]
	assert.Equal(t, []cluster.NodeID{"n1", "n2"}, got.Followers)
	assert.Empty(t, got.Learners)
	assert.Equal(t, cluster.NodeID("n0"), got.Owner,
		"promotion grants the right to be promoted, not ownership")
}

// TestUnfinishedLearnerIsLeftAlone: a learner holding part of a range is the one
// state that must not be served, and waiting costs nothing — the range keeps
// being served by its owner while the copy runs.
func TestUnfinishedLearnerIsLeftAlone(t *testing.T) {
	r := &readiness{ready: map[cluster.NodeID]bool{"n2": false}}
	f := movingFixture(t, r, "n0", "n1", "n2")

	out := f.reconcile(t)

	assert.False(t, out.Changed)
	assert.Empty(t, out.Learned)
	assert.Empty(t, f.ctl.acts, "nothing was written")

	assert.Equal(t, []cluster.NodeID{"n2"}, f.ctl.m.Ranges[0].Learners,
		"still a learner")
}

// TestUnreachableLearnerIsNotPromoted: unreachable and unfinished are the same
// answer. A learner the controller cannot reach is one it must not promote, and
// the pass must not fail over it either — a controller that stopped reconciling
// because one node was slow would stop failing over too.
func TestUnreachableLearnerIsNotPromoted(t *testing.T) {
	r := &readiness{fail: map[cluster.NodeID]bool{"n2": true}}
	f := movingFixture(t, r, "n0", "n1", "n2")

	out := f.reconcile(t)

	assert.False(t, out.Changed)
	assert.Empty(t, out.Learned)
	assert.Equal(t, []cluster.NodeID{"n2"}, f.ctl.m.Ranges[0].Learners)
}

// TestMissingLearnerIsNotAsked: a node that is not in the membership is not
// asked at all. Asking would be a round trip to a node that is gone, on the pass
// that runs most often.
func TestMissingLearnerIsNotAsked(t *testing.T) {
	r := &readiness{ready: map[cluster.NodeID]bool{"n2": true}}
	f := movingFixture(t, r, "n0", "n1")

	out := f.reconcile(t)

	assert.False(t, out.Changed)
	assert.Empty(t, r.asked, "a learner that is gone was asked anyway")
	assert.Equal(t, []cluster.NodeID{"n2"}, f.ctl.m.Ranges[0].Learners)
}

// TestPromotionIsIdempotent: a promoted node is a follower, and a follower is
// not asked again. Without that the controller would rewrite the map every pass
// for a move that finished.
func TestPromotionIsIdempotent(t *testing.T) {
	r := &readiness{ready: map[cluster.NodeID]bool{"n2": true}}
	f := movingFixture(t, r, "n0", "n1", "n2")

	require.True(t, f.reconcile(t).Changed)

	r.asked = nil

	out := f.reconcile(t)
	assert.False(t, out.Changed)
	assert.Empty(t, r.asked)
	assert.Len(t, f.ctl.acts, 1, "the map was written twice for one move")
}

// TestFailoverPassDoesNotPromote is the one-thing-per-pass rule. A promotion is
// a rewrite of the map, and one made from a map that failover has just
// invalidated would be describing a cluster that no longer exists.
func TestFailoverPassDoesNotPromote(t *testing.T) {
	r := &readiness{ready: map[cluster.NodeID]bool{"n2": true}}
	f := movingFixture(t, r, "n0", "n1", "n2")

	// The owner goes, and stays gone past its grace.
	f.live = []cluster.NodeID{"n1", "n2"}
	f.reconcile(t)
	f.clk.advance(time.Minute)

	out := f.reconcile(t)

	require.NotEmpty(t, out.Promoted, "the premise: this pass failed over")
	assert.Empty(t, out.Learned, "and promoted a learner in the same breath")
}

// TestPromotionDoesNotSplitInTheSamePass: the same rule in the other direction.
// A split planned from the map as it was would be applied to one the promotion
// had already changed.
func TestPromotionDoesNotSplitInTheSamePass(t *testing.T) {
	r := &readiness{ready: map[cluster.NodeID]bool{"n2": true}}
	f := movingFixture(t, r, "n0", "n1", "n2")

	measured := 0

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load,
		Save: f.ctl.save,
		Live: func() []cluster.NodeID { return f.live },
		Measure: func(context.Context, cluster.NodeID, rangemap.Range) (shardstore.Measurement, error) {
			measured++

			return shardstore.Measurement{Bytes: 1 << 40, SplitAt: "om"}, nil
		},
		Ready:     r.answer,
		Readiness: f.ctl,
		Now:       f.clk.now,
	})
	require.NoError(t, err)

	out, err := c.Reconcile(t.Context())
	require.NoError(t, err)

	require.Len(t, out.Learned, 1, "the premise: this pass promoted")
	assert.Empty(t, out.Split)
	assert.Zero(t, measured, "a split was planned from a map the promotion had changed")

	// And the next pass, with nothing left to promote, does split.
	out, err = c.Reconcile(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, out.Split)
}

// TestPromotionIsOffWithoutAWayToAsk: a plane with no transport cannot ask a
// learner anything, and a controller that promoted on no evidence would promote
// every learner the moment it was named one.
func TestPromotionIsOffWithoutAWayToAsk(t *testing.T) {
	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{{
		Start: "", End: "", Owner: "n0", Learners: []cluster.NodeID{"n2"},
	}}}
	require.NoError(t, m.Validate())

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: []cluster.NodeID{"n0", "n2"},
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live: func() []cluster.NodeID { return f.live },
		Now:  f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	out := f.reconcile(t)
	assert.False(t, out.Changed)
	assert.Equal(t, []cluster.NodeID{"n2"}, f.ctl.m.Ranges[0].Learners)
}

// TestAMoveRunsToCompletion is E4's move, whole, over real nodes and real HTTP:
// the controller records the move, the destination copies itself current, the
// controller asks it across the wire and promotes it, and the pass after that
// hands it the range.
//
// Every piece is covered on its own; this is the only place they have to agree.
// Each half passes its own tests while disagreeing with the other — a learner
// that never gets asked, an intent nothing acts on, a handover onto a node that
// holds half a range — and none of those show up until the sequence is run.
func TestAMoveRunsToCompletion(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 20)

	// The move is decided: n1 becomes a learner, and the intent is recorded.
	started, err := c.ctl.m.StartMove("", "n1")
	require.NoError(t, err)
	c.ctl.publish(t, started.Ranges...)

	ctl, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: c.ctl.load,
		Save: c.ctl.save,
		Live: func() []cluster.NodeID { return []cluster.NodeID{"n0", "n1"} },
		// Asked from n0, so the question crosses the wire to n1.
		Ready: c.nodes["n0"].plane.Ready,
		Now:   time.Now,
	})
	require.NoError(t, err)

	// Nothing yet: the copy has not run, so there is nothing to promote and
	// nothing to hand over.
	out, err := ctl.Reconcile(t.Context())
	require.NoError(t, err)
	require.Empty(t, out.Learned, "promoted before anything was copied")
	require.Empty(t, out.Moved)

	// The destination catches itself up.
	caught, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)
	require.Len(t, caught.Copied, 1)
	require.Equal(t, 20, caught.Entries)

	// One pass promotes it out of being a learner.
	out, err = ctl.Reconcile(t.Context())
	require.NoError(t, err)
	require.Len(t, out.Learned, 1)
	require.Empty(t, out.Moved, "promoted and handed over in the same pass")

	assert.Equal(t, cluster.NodeID("n0"), c.ctl.m.Ranges[0].Owner,
		"ownership moves in its own edit, after the node is an ordinary replica")

	// The next hands it the range.
	out, err = ctl.Reconcile(t.Context())
	require.NoError(t, err)
	require.Len(t, out.Moved, 1)

	got := c.ctl.m.Ranges[0]
	assert.Equal(t, cluster.NodeID("n1"), got.Owner)
	assert.Empty(t, got.MoveTo)
	assert.Equal(t, []cluster.NodeID{"n0"}, got.Followers)

	// And the cluster still answers for every object, now from the new owner.
	c.refreshAll(t)

	for i := range 20 {
		_, found, err := c.store("n0").Get(t.Context(), "photos", fmt.Sprintf("%03d.jpg", i))
		require.NoError(t, err)
		assert.True(t, found, "object %03d.jpg was lost in the move", i)
	}

	assert.Equal(t, []rangemap.Range{got}, c.nodes["n1"].shard.Ranges(),
		"the new owner serves the range it was given")
}

// handoverFixture is a controller over a map whose move has reached its last
// step: n0 owns, n1 has finished its copy and is a follower, and the intent to
// hand it the range is still recorded.
func handoverFixture(t *testing.T, live ...cluster.NodeID) *fixture {
	t.Helper()

	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{{
		Start: "", End: "", Owner: "n0",
		Followers: []cluster.NodeID{"n1"},
		MoveTo:    "n1",
	}}}
	require.NoError(t, m.Validate())

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: live,
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live:      func() []cluster.NodeID { return f.live },
		Readiness: f.ctl,
		Now:       f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	return f
}

// TestFinishedMoveIsHandedOver is the last edit of a move: the destination holds
// the range, so it takes it.
func TestFinishedMoveIsHandedOver(t *testing.T) {
	f := handoverFixture(t, "n0", "n1")

	out := f.reconcile(t)

	assert.True(t, out.Changed)
	require.Len(t, out.Moved, 1)

	got := f.ctl.m.Ranges[0]
	assert.Equal(t, cluster.NodeID("n1"), got.Owner)
	assert.Empty(t, got.MoveTo)
	assert.Equal(t, []cluster.NodeID{"n0"}, got.Followers,
		"the old owner keeps its copy: dropping it would lower R as ownership changes")
}

// TestHandoverIsIdempotent: the intent is cleared by the handover, so the next
// pass has nothing to do. Without that the controller would rewrite the map
// every tick for a move that finished.
func TestHandoverIsIdempotent(t *testing.T) {
	f := handoverFixture(t, "n0", "n1")

	require.True(t, f.reconcile(t).Changed)

	out := f.reconcile(t)
	assert.False(t, out.Changed)
	assert.Empty(t, out.Moved)
	assert.Len(t, f.ctl.acts, 1, "the map was written twice for one handover")
}

// TestHandoverWaitsForAnAbsentDestination: a destination that is gone holds a
// full copy and the range is still served by its current owner, so waiting costs
// nothing. Picking a different destination would throw away a finished copy to
// start another.
func TestHandoverWaitsForAnAbsentDestination(t *testing.T) {
	f := handoverFixture(t, "n0")

	out := f.reconcile(t)

	assert.False(t, out.Changed)
	assert.Empty(t, out.Moved)
	assert.Equal(t, cluster.NodeID("n0"), f.ctl.m.Ranges[0].Owner)
	assert.Equal(t, cluster.NodeID("n1"), f.ctl.m.Ranges[0].MoveTo, "the move is still recorded")
}

// TestUnfinishedMoveIsNotHandedOver: the destination is still a learner, so its
// copy is not done. Handing it the range would make it the owner of data it does
// not have.
func TestUnfinishedMoveIsNotHandedOver(t *testing.T) {
	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{{
		Start: "", End: "", Owner: "n0",
		Learners: []cluster.NodeID{"n1"},
		MoveTo:   "n1",
	}}}
	require.NoError(t, m.Validate())

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: []cluster.NodeID{"n0", "n1"},
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live: func() []cluster.NodeID { return f.live },
		Now:  f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	out := f.reconcile(t)
	assert.False(t, out.Changed)
	assert.Equal(t, cluster.NodeID("n0"), f.ctl.m.Ranges[0].Owner)
}

// TestHandoverComesBeforeStartingMoreWork: a range mid-move is holding an extra
// replica until it completes, so the pass that can finish one does that rather
// than promoting a learner somewhere else.
func TestHandoverComesBeforeStartingMoreWork(t *testing.T) {
	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{
		{Start: "", End: "om", Owner: "n0", Followers: []cluster.NodeID{"n1"}, MoveTo: "n1"},
		{Start: "om", End: "", Owner: "n0", Learners: []cluster.NodeID{"n2"}},
	}}
	require.NoError(t, m.Validate())

	r := &readiness{ready: map[cluster.NodeID]bool{"n2": true}}

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: []cluster.NodeID{"n0", "n1", "n2"},
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live:  func() []cluster.NodeID { return f.live },
		Ready: r.answer,
		Now:   f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	out, err := c.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Len(t, out.Moved, 1)
	assert.Empty(t, out.Learned, "a promotion and a handover in the same pass")

	// And the next pass promotes the other one.
	out, err = c.Reconcile(t.Context())
	require.NoError(t, err)
	assert.Len(t, out.Learned, 1)
}

// TestHandoverFinishesWhileTheOwnerIsAbsent: a range held because its owner is
// gone but still inside its grace is no reason to wait. The destination already
// holds everything the owner had, so handing it over resolves the absence
// instead of enduring it.
func TestHandoverFinishesWhileTheOwnerIsAbsent(t *testing.T) {
	f := handoverFixture(t, "n1")

	out := f.reconcile(t)

	require.Len(t, out.Moved, 1)

	got := f.ctl.m.Ranges[0]
	assert.Equal(t, cluster.NodeID("n1"), got.Owner, "the range is served again")
	assert.Empty(t, got.MoveTo)
}

// TestOrphanedOntoTheDestinationEndsTheMove: a range with no live follower is
// reassigned to whichever node is least loaded, and that can be the learner it
// was already moving to. Left recorded, the intent would name the owner as its
// own destination — which Validate refuses, and which would fail every later
// pass rather than the one that made it.
func TestOrphanedOntoTheDestinationEndsTheMove(t *testing.T) {
	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{{
		Start: "", End: "", Owner: "n0",
		Learners: []cluster.NodeID{"n1"},
		MoveTo:   "n1",
	}}}
	require.NoError(t, m.Validate())

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: []cluster.NodeID{"n1"},
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live:         func() []cluster.NodeID { return f.live },
		Readiness:    f.ctl,
		PromoteAfter: 30 * time.Second,
		RebuildAfter: 10 * time.Minute,
		Now:          f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	// The owner goes, and stays gone past the long grace: there is no follower
	// to promote, so the range is rebuilt onto whoever is left.
	f.reconcile(t)
	f.clk.advance(time.Hour)

	out := f.reconcile(t)
	require.Len(t, out.Orphaned, 1)

	got := f.ctl.m.Ranges[0]
	assert.Equal(t, cluster.NodeID("n1"), got.Owner)
	assert.Empty(t, got.MoveTo, "the intent outlived the move and would name its own owner")
	require.NoError(t, f.ctl.m.Validate())

	// And the passes that follow are quiet rather than failing on an invalid map.
	assert.False(t, f.reconcile(t).Changed)
}

// trimFixture is a controller over a range holding more copies than configured,
// which is the state a completed move leaves behind.
func trimFixture(t *testing.T, replicas int, r rangemap.Range) *fixture {
	t.Helper()

	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{r}}
	require.NoError(t, m.Validate())

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: []cluster.NodeID{"n0", "n1", "n2"},
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live:      func() []cluster.NodeID { return f.live },
		Readiness: f.ctl,
		Replicas:  replicas,
		Now:       f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	return f
}

// TestSurplusReplicaIsTrimmedAfterAMove is the debt a move leaves.
//
// Handover keeps the node it replaced as a follower rather than dropping it in
// the same edit, so a range comes out of a move holding one more copy than it
// went in with. Nothing gave it back until this: a cluster that rebalanced would
// gain a replica per move, forever.
func TestSurplusReplicaIsTrimmedAfterAMove(t *testing.T) {
	// What Handover leaves: n1 took the range, n0 is the node it replaced.
	f := trimFixture(t, 2, rangemap.Range{
		Start: "", End: "", Owner: "n1",
		Followers: []cluster.NodeID{"n2", "n0"},
	})

	out := f.reconcile(t)

	assert.True(t, out.Changed)
	require.Len(t, out.Trimmed, 1)

	assert.Equal(t, []cluster.NodeID{"n2"}, f.ctl.m.Ranges[0].Followers,
		"the node the range was moved off keeps a copy, so the move relieved nothing")
}

// TestTrimIsIdempotent: a range already at its replica count is left alone, or
// the controller would rewrite the map every tick forever.
func TestTrimIsIdempotent(t *testing.T) {
	f := trimFixture(t, 2, rangemap.Range{
		Start: "", End: "", Owner: "n1",
		Followers: []cluster.NodeID{"n2", "n0"},
	})

	require.True(t, f.reconcile(t).Changed)

	out := f.reconcile(t)
	assert.False(t, out.Changed)
	assert.Empty(t, out.Trimmed)
	assert.Len(t, f.ctl.acts, 1, "the map was written twice for one trim")
}

// TestTrimLeavesARangeMidMoveAlone: a range that is still moving is *supposed*
// to have an extra copy — that is what the copy is. Trimming it would take away
// the replica the move is in the middle of making.
func TestTrimLeavesARangeMidMoveAlone(t *testing.T) {
	f := trimFixture(t, 2, rangemap.Range{
		Start: "", End: "", Owner: "n0",
		Followers: []cluster.NodeID{"n2", "n1"},
		MoveTo:    "n1",
	})

	// The destination is away, so the handover cannot run and the surplus sits
	// there for the trim to find. Without the guard it drops n1 — the very node
	// the range is being given to — and the map stops being valid at all.
	f.live = []cluster.NodeID{"n0", "n2"}

	out := f.reconcile(t)

	assert.Empty(t, out.Trimmed)
	assert.Equal(t, []cluster.NodeID{"n2", "n1"}, f.ctl.m.Ranges[0].Followers,
		"the move lost the replica it was making")
	assert.Equal(t, cluster.NodeID("n1"), f.ctl.m.Ranges[0].MoveTo)
}

// TestTrimNeverGoesBelowTheTarget: a range short of its replicas is not the
// trim's business, and taking one away would be the opposite of what it is for.
func TestTrimNeverGoesBelowTheTarget(t *testing.T) {
	// Exactly at the target: two replicas, so one follower. One fewer would be
	// a range kept below what the cluster asked for, by the pass that exists to
	// keep it at it.
	f := trimFixture(t, 2, rangemap.Range{
		Start: "", End: "", Owner: "n0",
		Followers: []cluster.NodeID{"n1"},
	})

	out := f.reconcile(t)

	assert.False(t, out.Changed)
	assert.Equal(t, []cluster.NodeID{"n1"}, f.ctl.m.Ranges[0].Followers)
}

// TestTrimIsOffWithoutATarget: a caller that does not know how many copies the
// cluster keeps must not have this guess. Dropping replicas on a guess is the
// one mistake here that costs data rather than space.
func TestTrimIsOffWithoutATarget(t *testing.T) {
	f := trimFixture(t, 0, rangemap.Range{
		Start: "", End: "", Owner: "n1",
		Followers: []cluster.NodeID{"n2", "n0"},
	})

	out := f.reconcile(t)

	assert.False(t, out.Changed)
	assert.Len(t, f.ctl.m.Ranges[0].Followers, 2)
}

// TestTheNodeAMoveRelievesDoesNotKeepACopy is the debt and its repayment in
// sequence: the handover leaves the range one copy over, and the pass after it
// takes that copy from the node the range was moved off.
//
// Which node is dropped is the whole point. A range is moved to relieve its
// owner; leaving the replica there means its disk still holds the range, so the
// move relieved nothing it was meant to.
//
// The two do not share a pass, though nothing would break if they did — trim
// skips a range that is still moving, so the order is a preference for finishing
// work in flight rather than a rule that has to hold.
func TestTheNodeAMoveRelievesDoesNotKeepACopy(t *testing.T) {
	m := &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{{
		Start: "", End: "", Owner: "n0",
		Followers: []cluster.NodeID{"n2", "n1"},
		MoveTo:    "n1",
	}}}
	require.NoError(t, m.Validate())

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: []cluster.NodeID{"n0", "n1", "n2"},
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: f.ctl.load, Save: f.ctl.save,
		Live:     func() []cluster.NodeID { return f.live },
		Replicas: 2,
		Now:      f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	out, err := c.Reconcile(t.Context())
	require.NoError(t, err)
	require.Len(t, out.Moved, 1)
	assert.Empty(t, out.Trimmed, "trimmed in the same pass as the handover")

	// n1 owns it now, and n0 — the node it was moved off — is the surplus.
	require.Equal(t, cluster.NodeID("n1"), f.ctl.m.Ranges[0].Owner)
	require.Equal(t, []cluster.NodeID{"n2", "n0"}, f.ctl.m.Ranges[0].Followers)

	out, err = c.Reconcile(t.Context())
	require.NoError(t, err)
	require.Len(t, out.Trimmed, 1)

	assert.Equal(t, []cluster.NodeID{"n2"}, f.ctl.m.Ranges[0].Followers,
		"the node the range was moved off is the one that kept a copy")
}
