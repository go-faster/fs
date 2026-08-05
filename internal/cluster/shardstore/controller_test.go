package shardstore_test

import (
	"context"
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

// recorder is a control plane that remembers the order it was written in.
//
// Order rather than just contents, because one of the things worth asserting is
// a sequence: the plane must be marked building *before* a map with an orphaned
// range is published, or a node picks that map up while the plane still reads
// ready and serves an empty range as authoritative.
type recorder struct {
	m *rangemap.Map

	build metastore.Build
	acts  []string

	saveErr error
}

func (r *recorder) load(context.Context) (*rangemap.Map, error) { return r.m, nil }

func (r *recorder) save(_ context.Context, m *rangemap.Map) error {
	if r.saveErr != nil {
		return r.saveErr
	}

	r.acts = append(r.acts, "save")
	r.m = m

	return nil
}

func (r *recorder) Status(context.Context) (metastore.Build, error) { return r.build, nil }

func (r *recorder) Set(_ context.Context, b metastore.Build) error {
	r.acts = append(r.acts, "state:"+stateName(b))
	r.build = b

	return nil
}

// stateName is what the recorder writes into its log: "ready", or "building"
// with the reason, so a test can assert the cause reached the flag.
func stateName(b metastore.Build) string {
	if b.State == metastore.StateReady {
		return "ready"
	}

	return "building:" + b.Cause.String()
}

// clock is a hand-wound clock, so grace periods are tested by moving time
// rather than by waiting for it.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fixture is a controller over a recorder, with membership a test can change.
type fixture struct {
	ctl  *recorder
	clk  *clock
	live []cluster.NodeID
	c    *shardstore.Controller
}

func newFixture(t *testing.T, m *rangemap.Map, live ...cluster.NodeID) *fixture {
	t.Helper()

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
		PromoteAfter: 30 * time.Second,
		RebuildAfter: 10 * time.Minute,
		Now:          f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	return f
}

func (f *fixture) reconcile(t *testing.T) shardstore.Reconciliation {
	t.Helper()

	out, err := f.c.Reconcile(t.Context())
	require.NoError(t, err)

	return out
}

// replicatedMap is one range owned by n0 with n1 following.
func replicatedMap() *rangemap.Map {
	return &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{
		{Start: "", End: "", Owner: "n0", Followers: []cluster.NodeID{"n1"}},
	}}
}

// soloMap is one range owned by n0 with nobody following: R=1, so a lost owner
// costs a rebuild rather than a promotion.
func soloMap() *rangemap.Map {
	return &rangemap.Map{Revision: 7, Ranges: []rangemap.Range{
		{Start: "", End: "", Owner: "n0"},
	}}
}

// TestHealthyClusterWritesNothing: a pass where every owner is alive must cost
// a read and nothing else. This runs on a timer forever, so a pass that wrote
// unconditionally would put a control-plane write per node per tick underneath
// a cluster that has nothing wrong with it.
func TestHealthyClusterWritesNothing(t *testing.T) {
	f := newFixture(t, replicatedMap(), "n0", "n1")

	for range 5 {
		out := f.reconcile(t)
		assert.False(t, out.Changed)
		f.clk.advance(time.Hour)
	}

	assert.Empty(t, f.ctl.acts, "nothing was written")
}

// TestPromotionWaitsForItsGrace: a failover is not free — the range is unserved
// from the moment the owner goes until the new one picks the map up — but it is
// cheap, so the wait is short.
func TestPromotionWaitsForItsGrace(t *testing.T) {
	f := newFixture(t, replicatedMap(), "n0", "n1")

	f.reconcile(t) // notices n0 while it is still alive

	f.live = []cluster.NodeID{"n1"}

	out := f.reconcile(t)
	require.False(t, out.Changed, "the absence has not been timed yet")
	require.Len(t, out.Held, 1)

	f.clk.advance(29 * time.Second)

	out = f.reconcile(t)
	assert.False(t, out.Changed, "still inside the grace")

	f.clk.advance(2 * time.Second)

	out = f.reconcile(t)
	require.True(t, out.Changed)
	require.Len(t, out.Promoted, 1)
	assert.Empty(t, out.Orphaned)
	assert.Equal(t, cluster.NodeID("n1"), f.ctl.m.Ranges[0].Owner)
	assert.Equal(t, []string{"save"}, f.ctl.acts, "a promotion does not make the plane untrustworthy")
}

// TestAnOrphanWaitsMuchLonger is the distinction the two grace periods exist
// for, and the one that decides what a node reboot costs.
//
// With a follower there is something to promote, so the range moves in seconds.
// Without one the new owner starts empty and the whole plane must be walked off
// the disks before it can be trusted — so waiting for the owner to come back is
// the cheaper of the two bad options, by a wide margin.
func TestAnOrphanWaitsMuchLonger(t *testing.T) {
	f := newFixture(t, soloMap(), "n0", "n1")

	f.reconcile(t)

	f.live = []cluster.NodeID{"n1"}
	f.reconcile(t)

	// Well past the promotion grace, which a replicated range would have used.
	f.clk.advance(5 * time.Minute)

	out := f.reconcile(t)
	require.False(t, out.Changed, "no follower, so no cheap answer, so no hurry")
	require.Len(t, out.Held, 1)
	assert.Empty(t, f.ctl.acts)

	// And the reward for waiting: n0 comes back, and nothing was rebuilt.
	f.live = []cluster.NodeID{"n0", "n1"}

	out = f.reconcile(t)
	assert.False(t, out.Changed)
	assert.Empty(t, out.Held)
	assert.Equal(t, metastore.Ready(), f.ctl.build)
}

// TestOrphanMarksBuildingBeforePublishingTheMap: order is the assertion.
//
// A node that picked up a map with an orphaned range while the plane still read
// ready would serve that range as authoritative, and an empty range answers
// "no such object" for objects that exist — a wrong answer rather than a slow
// one. Building first makes listings fall back to the sidecar walk, which is
// slower and always right.
func TestOrphanMarksBuildingBeforePublishingTheMap(t *testing.T) {
	f := newFixture(t, soloMap(), "n0", "n1")

	f.reconcile(t)

	f.live = []cluster.NodeID{"n1"}
	f.reconcile(t)

	f.clk.advance(11 * time.Minute)

	out := f.reconcile(t)
	require.True(t, out.Changed)
	require.Len(t, out.Orphaned, 1)
	assert.True(t, out.RebuildOwed())

	assert.Equal(t, []string{"state:building:orphaned", "save"}, f.ctl.acts,
		"building is published before the map that makes it true")

	assert.Equal(t, cluster.NodeID("n1"), f.ctl.m.Ranges[0].Owner,
		"a range with no owner is a key nothing serves, which is worse")
}

// TestFlappingCostsNothing: each absence starts its own clock. A node that is
// gone for twenty seconds twice must never be treated as gone for forty — that
// is how a rolling restart turns into a plane-wide rebuild.
func TestFlappingCostsNothing(t *testing.T) {
	f := newFixture(t, replicatedMap(), "n0", "n1")

	for range 10 {
		f.reconcile(t)

		f.live = []cluster.NodeID{"n1"}
		f.reconcile(t)
		f.clk.advance(20 * time.Second)
		f.reconcile(t)

		f.live = []cluster.NodeID{"n0", "n1"}
		f.clk.advance(20 * time.Second)
	}

	assert.Empty(t, f.ctl.acts, "n0 was never gone for a whole grace period at once")
	assert.Equal(t, cluster.NodeID("n0"), f.ctl.m.Ranges[0].Owner)
}

// spreadMap is four ranges on four owners, so a test can take some away and
// leave the rest.
func spreadMap() *rangemap.Map {
	return &rangemap.Map{Revision: 3, Ranges: []rangemap.Range{
		{Start: "", End: "ob", Owner: "n0", Followers: []cluster.NodeID{"n1"}},
		{Start: "ob", End: "oc", Owner: "n1", Followers: []cluster.NodeID{"n2"}},
		{Start: "oc", End: "od", Owner: "n2", Followers: []cluster.NodeID{"n3"}},
		{Start: "od", End: "", Owner: "n3", Followers: []cluster.NodeID{"n0"}},
	}}
}

// TestMassDisappearanceIsRefused: a controller that cannot see most of the
// cluster is misreading the registry far more often than it is watching most of
// the cluster fail, and the two look identical from here.
//
// Reassigning everything at once is useless in the second case — there is
// nowhere to put it — and catastrophic in the first, because it orphans ranges
// whose owners are fine and bills a full rebuild for a control-plane blip.
func TestMassDisappearanceIsRefused(t *testing.T) {
	f := newFixture(t, spreadMap(), "n0", "n1", "n2", "n3")

	f.reconcile(t)

	// Three of four owners vanish at once, which no failure mode this plane is
	// designed for produces.
	f.live = []cluster.NodeID{"n3"}
	f.clk.advance(time.Hour)

	_, err := f.c.Reconcile(t.Context())
	require.Error(t, err)
	assert.Empty(t, f.ctl.acts, "nothing was written")
	assert.Equal(t, cluster.NodeID("n0"), f.ctl.m.Ranges[0].Owner)

	// One node failing is a failure. It is handled.
	f.live = []cluster.NodeID{"n1", "n2", "n3"}
	f.reconcile(t)
	f.clk.advance(time.Minute)

	out := f.reconcile(t)
	require.True(t, out.Changed)
	assert.Len(t, out.Promoted, 1)
}

// TestEmptyMembershipIsRefused is the extreme of the same thing: a membership
// with nobody in it. The node running this is registered, so it should at least
// see itself.
func TestEmptyMembershipIsRefused(t *testing.T) {
	f := newFixture(t, replicatedMap(), "n0", "n1")

	f.reconcile(t)
	f.clk.advance(time.Hour)

	f.live = nil

	_, err := f.c.Reconcile(t.Context())
	require.Error(t, err)
	assert.Empty(t, f.ctl.acts, "nothing was written")
	assert.Equal(t, cluster.NodeID("n0"), f.ctl.m.Ranges[0].Owner)
}

// TestTwoControllersAgree: an election has a window where two candidates both
// believe they hold it. The reconciliation is deterministic so that window
// produces a duplicate write rather than a disagreement — two controllers
// reacting to one failure must compute the same map.
func TestTwoControllersAgree(t *testing.T) {
	build := func() *rangemap.Map {
		return &rangemap.Map{Revision: 3, Ranges: []rangemap.Range{
			{Start: "", End: "ob", Owner: "gone"},
			{Start: "ob", End: "oc", Owner: "gone"},
			{Start: "oc", End: "od", Owner: "gone"},
			{Start: "od", End: "oe", Owner: "n0"},
			{Start: "oe", End: "of", Owner: "n1"},
			{Start: "of", End: "", Owner: "n3"},
		}}
	}

	run := func() *rangemap.Map {
		f := newFixture(t, build(), "gone", "n0", "n1", "n2", "n3")

		f.reconcile(t)

		f.live = []cluster.NodeID{"n0", "n1", "n2", "n3"}
		f.reconcile(t)
		f.clk.advance(11 * time.Minute)

		out := f.reconcile(t)
		require.True(t, out.Changed)
		require.Len(t, out.Orphaned, 3)

		return f.ctl.m
	}

	first, second := run(), run()

	require.Equal(t, first, second)
	assert.NotEqual(t, first.Ranges[0].Owner, first.Ranges[2].Owner,
		"the orphans spread rather than piling onto whoever was least loaded first")
}

// TestAFailedWriteIsNotReportedAsDone: a caller acts on Reconciliation —
// running a rebuild, logging a promotion — and doing that for a map that was
// never published would have it believe a failover happened that did not.
func TestAFailedWriteIsNotReportedAsDone(t *testing.T) {
	f := newFixture(t, replicatedMap(), "n0", "n1")

	f.reconcile(t)

	f.live = []cluster.NodeID{"n1"}
	f.reconcile(t)
	f.clk.advance(time.Minute)

	f.ctl.saveErr = errors.New("etcd is unreachable")

	out, err := f.c.Reconcile(t.Context())
	require.Error(t, err)
	assert.False(t, out.Changed)
	assert.Equal(t, cluster.NodeID("n0"), f.ctl.m.Ranges[0].Owner)
}

// TestUnpartitionedPlaneIsRefused: bootstrap is Initialize's job, and a
// controller that partitioned an empty map would be guessing at a layout while
// reacting to a failure.
func TestUnpartitionedPlaneIsRefused(t *testing.T) {
	ctl := &recorder{m: &rangemap.Map{}, build: metastore.Ready()}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: ctl.load,
		Save: ctl.save,
		Live: func() []cluster.NodeID { return []cluster.NodeID{"n0"} },
	})
	require.NoError(t, err)

	_, err = c.Reconcile(t.Context())
	require.Error(t, err)
	assert.Empty(t, ctl.acts)
}

// sized backs a controller's Measure from a table keyed by range start.
type sized struct {
	byStart map[string]shardstore.Measurement
	calls   int
}

func (s *sized) measure(
	_ context.Context, _ cluster.NodeID, r rangemap.Range,
) (shardstore.Measurement, error) {
	s.calls++

	return s.byStart[r.Start], nil
}

// splitting builds a controller whose membership is healthy and whose ranges
// measure as the table says.
func splitting(t *testing.T, m *rangemap.Map, table *sized, live ...cluster.NodeID) *fixture {
	t.Helper()

	require.NoError(t, m.Validate())

	f := &fixture{
		ctl:  &recorder{m: m, build: metastore.Ready()},
		clk:  &clock{t: time.Unix(1700000000, 0)},
		live: live,
	}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load:      f.ctl.load,
		Save:      f.ctl.save,
		Live:      func() []cluster.NodeID { return f.live },
		Readiness: f.ctl,
		Measure:   table.measure,
		Split:     shardstore.SplitPolicy{MaxBytes: 1000, MaxSplitsPerPass: 4},
		Now:       f.clk.now,
	})
	require.NoError(t, err)

	f.c = c

	return f
}

// TestControllerSplitsAnOversizedRange is the loop closing: E3 shipped a
// partitioning nothing ever changed, and this is what changes it.
func TestControllerSplitsAnOversizedRange(t *testing.T) {
	table := &sized{byStart: map[string]shardstore.Measurement{
		"": {Bytes: 9000, SplitAt: "om"},
	}}

	f := splitting(t, &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "", Owner: "n0", Followers: []cluster.NodeID{"n1"}},
	}}, table, "n0", "n1")

	out := f.reconcile(t)
	require.True(t, out.Changed)
	assert.Equal(t, []string{"om"}, out.Split)

	require.Len(t, f.ctl.m.Ranges, 2)
	assert.Equal(t, "om", f.ctl.m.Ranges[1].Start)
	assert.Equal(t, []string{"save"}, f.ctl.acts,
		"a split does not make the plane untrustworthy: it moves nothing")

	// Both halves keep the owner and the followers, so nothing has to be copied
	// before the map is published.
	for i, r := range f.ctl.m.Ranges {
		assert.Equal(t, cluster.NodeID("n0"), r.Owner, "half %d", i)
		assert.Equal(t, []cluster.NodeID{"n1"}, r.Followers, "half %d", i)
	}
}

// TestControllerLeavesASettledPartitioningAlone: a pass where nothing is wrong
// and nothing is oversized must write nothing. This runs on a timer forever.
func TestControllerLeavesASettledPartitioningAlone(t *testing.T) {
	table := &sized{byStart: map[string]shardstore.Measurement{
		"": {Bytes: 10, SplitAt: "om"},
	}}

	f := splitting(t, &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "", Owner: "n0"},
	}}, table, "n0")

	for range 3 {
		out := f.reconcile(t)
		assert.False(t, out.Changed)
		assert.Empty(t, out.Split)
	}

	assert.Empty(t, f.ctl.acts)
	assert.Positive(t, table.calls, "it did look")
}

// TestFailoverAndSplitDoNotShareAPass: a pass that failed over *and* split would
// be deciding where data goes from measurements taken while it was moving, and
// the second decision would be made against a map the first had invalidated.
//
// Splits are not urgent and the next pass is seconds away.
func TestFailoverAndSplitDoNotShareAPass(t *testing.T) {
	// Small to begin with, so the warm-up passes that notice n0 do not split
	// first — which is what the earlier version of this test let happen, and
	// why it failed on a range count rather than on the claim.
	table := &sized{byStart: map[string]shardstore.Measurement{
		"": {Bytes: 10, SplitAt: "om"},
	}}

	f := splitting(t, &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "", Owner: "n0", Followers: []cluster.NodeID{"n1"}},
	}}, table, "n0", "n1")

	f.reconcile(t) // notices n0 while it is alive

	f.live = []cluster.NodeID{"n1"}
	f.reconcile(t)
	f.clk.advance(time.Minute)

	// It grew while the owner was away, so this pass has both jobs available.
	table.byStart[""] = shardstore.Measurement{Bytes: 9000, SplitAt: "om"}

	out := f.reconcile(t)
	require.True(t, out.Changed)
	require.Len(t, out.Promoted, 1)
	assert.Empty(t, out.Split, "the failover pass did not also split")
	assert.Len(t, f.ctl.m.Ranges, 1)

	// And the next pass, with the membership settled, does the split.
	next := f.reconcile(t)
	require.True(t, next.Changed)
	assert.Equal(t, []string{"om"}, next.Split)
	assert.Len(t, f.ctl.m.Ranges, 2)
	assert.Equal(t, cluster.NodeID("n1"), f.ctl.m.Ranges[0].Owner,
		"the promotion held; the split did not undo it")
}

// TestSplittingIsOffWithoutAWayToMeasure: a plane with no transport cannot ask
// an owner anything, and guessing would split on numbers nobody produced.
func TestSplittingIsOffWithoutAWayToMeasure(t *testing.T) {
	ctl := &recorder{m: &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "", Owner: "n0"},
	}}, build: metastore.Ready()}

	c, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: ctl.load,
		Save: ctl.save,
		Live: func() []cluster.NodeID { return []cluster.NodeID{"n0"} },
	})
	require.NoError(t, err)

	out, err := c.Reconcile(t.Context())
	require.NoError(t, err)
	assert.False(t, out.Changed)
	assert.Empty(t, ctl.acts)
}

// TestTwoBoundariesInOneRange: the plan is computed from one map, so two
// boundaries can name the same range — and the first split invalidates the
// second's view of it. The second is skipped rather than fatal, and the range is
// measured again next pass.
func TestTwoBoundariesInOneRange(t *testing.T) {
	table := &sized{byStart: map[string]shardstore.Measurement{
		"":   {Bytes: 9000, SplitAt: "om"},
		"ot": {Bytes: 8000, SplitAt: "ow"},
	}}

	f := splitting(t, &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "ot", Owner: "n0"},
		{Start: "ot", End: "", Owner: "n0"},
	}}, table, "n0")

	out := f.reconcile(t)
	require.True(t, out.Changed)
	assert.Equal(t, []string{"om", "ow"}, out.Split)
	assert.Len(t, f.ctl.m.Ranges, 4)
	require.NoError(t, f.ctl.m.Validate())
}

// TestABogusBoundaryIsSkippedNotFatal: the boundary arrives over the wire from
// the range's owner, so it is a number this node did not compute and cannot
// assume anything about.
//
// A peer that is buggy, mid-upgrade, or working from a map older than this one
// can name a key that is already a boundary. Failing the pass would let one
// such node stop the whole plane from ever splitting again; writing the map
// anyway would publish a revision for a change that did not happen.
func TestABogusBoundaryIsSkippedNotFatal(t *testing.T) {
	// "ob" is already where the second range starts, so splitting there is a
	// no-op the map surgery refuses.
	table := &sized{byStart: map[string]shardstore.Measurement{
		"":   {Bytes: 9000, SplitAt: "ob"},
		"ob": {Bytes: 9000, SplitAt: "ob"},
	}}

	f := splitting(t, &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "ob", Owner: "n0"},
		{Start: "ob", End: "", Owner: "n0"},
	}}, table, "n0")

	out, err := f.c.Reconcile(t.Context())
	require.NoError(t, err, "one bad peer must not stop the plane from splitting")
	assert.False(t, out.Changed)
	assert.Empty(t, out.Split)
	assert.Empty(t, f.ctl.acts, "no revision published for a change that did not happen")
	assert.Len(t, f.ctl.m.Ranges, 2)

	// And a good boundary alongside a bad one still lands.
	table.byStart["ob"] = shardstore.Measurement{Bytes: 9000, SplitAt: "om"}

	out = f.reconcile(t)
	require.True(t, out.Changed)
	assert.Equal(t, []string{"om"}, out.Split)
	assert.Len(t, f.ctl.m.Ranges, 3)
}

// TestOneWriteForAWholePassOfSplits: every split is a map revision each node in
// the cluster refetches, so a pass that published one per boundary would turn a
// tuned partitioning into a burst of routing traffic.
func TestOneWriteForAWholePassOfSplits(t *testing.T) {
	table := &sized{byStart: map[string]shardstore.Measurement{
		"":   {Bytes: 9000, SplitAt: "oam"},
		"ob": {Bytes: 9000, SplitAt: "obm"},
		"oc": {Bytes: 9000, SplitAt: "ocm"},
	}}

	f := splitting(t, &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "ob", Owner: "n0"},
		{Start: "ob", End: "oc", Owner: "n0"},
		{Start: "oc", End: "", Owner: "n0"},
	}}, table, "n0")

	out := f.reconcile(t)
	require.True(t, out.Changed)
	require.Len(t, out.Split, 3)

	assert.Equal(t, []string{"save"}, f.ctl.acts,
		"three boundaries, one published map")
	assert.Len(t, f.ctl.m.Ranges, 6)
}

// TestOrphanRecordsWhyThePlaneIsBuilding: the flag says the plane is not usable;
// the cause says whether anything should act on that without being asked.
//
// A failure that leaves a range with no copy of its data is degraded now, and
// every listing in the cluster is on the slow path until it is rebuilt. A plane
// that had merely never been built is waiting for an operator's window. Only the
// controller knows which of those just happened, so only the controller can say.
func TestOrphanRecordsWhyThePlaneIsBuilding(t *testing.T) {
	f := newFixture(t, soloMap(), "n1")

	f.reconcile(t)
	f.clk.advance(time.Hour)

	out := f.reconcile(t)
	require.Len(t, out.Orphaned, 1)

	assert.Equal(t, metastore.Building(metastore.CauseOrphaned), f.ctl.build,
		"the plane is building for a reason nothing can act on")
}
