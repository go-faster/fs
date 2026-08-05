package shardstore_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// owned is the whole key space on n0, with nobody replicating it.
var owned = rangemap.Range{Start: "", End: "", Owner: "n0"}

// learnedByN1 is owned with n1 being copied into.
func learnedByN1() rangemap.Range {
	r := owned
	r.Learners = []cluster.NodeID{"n1"}

	return r
}

// seed writes n objects through the cluster before anyone learns the range, so
// what a backfill copies is data the log never shipped.
//
// The distinction matters: a learner named in the map before the writes would
// receive them as batches, and a test that copied them anyway would pass with
// the backfill doing nothing at all.
func seed(t *testing.T, c *testCluster, n int) {
	t.Helper()

	for i := range n {
		require.NoError(t, c.store("n0").Put(t.Context(),
			entry("photos", fmt.Sprintf("%03d.jpg", i), 100, 1)))
	}
}

// held is what a node's shard holds for a bucket, regardless of what it serves.
func held(t *testing.T, c *testCluster, id cluster.NodeID, bucket string) []string {
	t.Helper()

	c.nodes[id].shard.Adopt([]rangemap.Range{owned})
	defer c.refreshAll(t)

	return scanKeys(t, c.nodes[id].shard, bucket)
}

// TestCatchUpCopiesALearnedRange is the move's data half running by itself: the
// controller names a learner, and the learner does the rest without being told
// again.
func TestCatchUpCopiesALearnedRange(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 20)

	c.ctl.publish(t, learnedByN1())

	got, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, got.Learning)
	assert.Len(t, got.Copied, 1)
	assert.Len(t, got.Ready, 1)
	assert.Empty(t, got.Failed)
	assert.Equal(t, 20, got.Entries)

	assert.Len(t, held(t, c, "n1", "photos"), 20)
}

// TestCatchUpDoesNotRecopyAFinishedRange: a learner stays a learner until the
// controller promotes it, so a pass that forgot what it had copied would re-read
// the whole range from its owner every tick.
func TestCatchUpDoesNotRecopyAFinishedRange(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 20)

	c.ctl.publish(t, learnedByN1())

	_, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	got, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	assert.Empty(t, got.Copied, "the range was copied a second time")
	assert.Zero(t, got.Entries, "the owner was read again for a range already held")
	assert.Len(t, got.Ready, 1, "and it is still ready to be promoted")
}

// TestCatchUpIgnoresARangeItMerelyFollows: a follower is current from the log by
// definition, and backfilling one would read every range this node replicates
// out of its owner on every pass — the cost the learner/follower split exists to
// confine to a move.
func TestCatchUpIgnoresARangeItMerelyFollows(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 12)

	followed := owned
	followed.Followers = []cluster.NodeID{"n1"}
	c.ctl.publish(t, followed)

	got, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	assert.Zero(t, got.Learning)
	assert.Empty(t, got.Copied)
	assert.Empty(t, held(t, c, "n1", "photos"),
		"a follower was backfilled, which is a learner's work")
}

// TestCatchUpForgetsARangeItNoLongerLearns is why the memory is rebuilt each
// pass rather than added to.
//
// A node promoted out of a range and later made a learner of it again is a node
// that may have dropped what it held in between. Remembering the first copy
// would have it report ready without copying anything — a promotion onto data it
// does not have.
func TestCatchUpForgetsARangeItNoLongerLearns(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 8)

	c.ctl.publish(t, learnedByN1())

	_, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	// Promoted: a follower now, not a learner.
	promoted := owned
	promoted.Followers = []cluster.NodeID{"n1"}
	c.ctl.publish(t, promoted)

	got, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)
	require.Zero(t, got.Learning)

	// And made a learner of it again.
	c.ctl.publish(t, learnedByN1())

	got, err = c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	assert.Len(t, got.Copied, 1,
		"a completion from before the node stopped learning the range was reused")
}

// TestCatchUpRecopiesWhenTheOwnerChanged: a copy is a copy of a particular
// node's contents. After a failover the range has the same bounds and different
// contents — the promoted node holds the records it received and not the ones it
// did not — and a completion remembered against the old owner would skip the
// difference.
func TestCatchUpRecopiesWhenTheOwnerChanged(t *testing.T) {
	c := newCluster(t, "n0", "n1", "n2")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 8)

	c.ctl.publish(t, learnedByN1())

	_, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	moved := learnedByN1()
	moved.Owner = "n2"
	c.ctl.publish(t, moved)
	c.refreshAll(t)

	got, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	assert.Len(t, got.Copied, 1,
		"a completion taken from a node that no longer owns the range was reused")
}

// TestCatchUpReportsAnUnreachableOwner: a move whose source is down is stuck,
// not finished. Reporting it ready would promote a learner holding a fraction of
// its range.
func TestCatchUpReportsAnUnreachableOwner(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 12)

	c.ctl.publish(t, learnedByN1())
	c.nodes["n0"].down = true

	got, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err, "one unreachable owner is not a failed pass")

	assert.Equal(t, 1, got.Learning)
	assert.Len(t, got.Failed, 1)
	assert.Empty(t, got.Ready, "a range that could not be copied is not ready")
}

// TestCatchUpResumesOnceTheOwnerIsBack: the failure is recorded, not remembered.
// The next pass picks up from the cursor, which is what makes an owner that
// flapped cost a step rather than a range.
func TestCatchUpResumesOnceTheOwnerIsBack(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 12)

	c.ctl.publish(t, learnedByN1())
	c.nodes["n0"].down = true

	_, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	c.nodes["n0"].down = false

	got, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	assert.Len(t, got.Copied, 1)
	assert.Len(t, held(t, c, "n1", "photos"), 12)
}

// TestCatchUpDoesNothingOnANodeLearningNothing is the ordinary case, and it must
// be free: every node runs this pass, and almost none of them are mid-move.
func TestCatchUpDoesNothingOnANodeLearningNothing(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, owned)
	c.refreshAll(t)

	seed(t, c, 4)

	got, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.NoError(t, err)

	assert.Zero(t, got.Learning)
	assert.Empty(t, got.Copied)
	assert.Empty(t, got.Ready)
	assert.Empty(t, got.Failed)
}

// TestCatchUpReportsAMapItCannotRead: a pass that could not read the map has not
// decided that this node learns nothing — it has decided nothing at all.
func TestCatchUpReportsAMapItCannotRead(t *testing.T) {
	c := newCluster(t, "n0", "n1")
	c.ctl.publish(t, learnedByN1())
	c.refreshAll(t)

	c.ctl.mu.Lock()
	c.ctl.fail = assert.AnError
	c.ctl.mu.Unlock()

	_, err := c.nodes["n1"].plane.CatchUp(t.Context())
	require.Error(t, err)
}
