package shardstore

import (
	"context"
	"slices"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// Default grace periods. See ControllerConfig for why there are two.
const (
	DefaultPromoteAfter = 30 * time.Second
	DefaultRebuildAfter = 15 * time.Minute
)

// DefaultMaxChurn is the share of range owners that may be missing before a
// reconciliation refuses to run. See Controller.churn.
const DefaultMaxChurn = 0.5

// churnFloor is the number of missing owners below which the share ceiling does
// not apply at all.
//
// One or two owners going away is a node failure at any cluster size, and it is
// what the promote-or-rebuild path exists to handle. Only once several have
// gone at once is "most of the plane is missing" a claim worth doubting.
const churnFloor = 3

// ControllerConfig configures the reconciler that keeps the partitioning
// consistent with who is alive.
type ControllerConfig struct {
	// Load and Save read and write the partitioning. In a node these are
	// closures over etcd.LoadRangeMap and etcd.SaveRangeMap.
	Load Loader
	Save func(ctx context.Context, m *rangemap.Map) error

	// Live reports the nodes currently registered. In a node it reads the
	// topology source, whose membership is etcd-lease-backed: a node that lost
	// its lease is gone, not merely quiet.
	Live func() []cluster.NodeID

	// Measure asks a range's owner what it holds and where it would divide.
	// Nil disables splitting, which is what a plane with no transport gets.
	Measure func(context.Context, cluster.NodeID, rangemap.Range) (Measurement, error)

	// Split is when a range is too large to leave alone.
	Split SplitPolicy

	// Rebalance is when a range is worth moving to relieve its owner. Nil
	// Measure disables it, the same way it disables splitting: neither can be
	// decided without asking the owners.
	Rebalance RebalancePolicy

	// Replicas is how many copies of a range the cluster is configured to keep,
	// counting the owner. Zero means surplus replicas are left alone, which is
	// what a caller that does not know the target should get.
	Replicas int

	// Ready asks a learner whether the copy of a range into it has finished.
	// Nil disables promotion, which is what a plane with no transport gets —
	// and what leaves a learner a learner forever, which is the safe direction.
	Ready func(context.Context, cluster.NodeID, rangemap.Range) (bool, error)

	// Readiness is the cluster-wide build flag. Orphaning a range makes the
	// plane untrustworthy until it is rebuilt, and this is what says so.
	Readiness Readiness

	// PromoteAfter is how long an owner must be gone before a range with a live
	// follower moves to it. Short: promotion costs one metadata write, and the
	// range is unserved until it happens.
	//
	// Zero means DefaultPromoteAfter.
	PromoteAfter time.Duration

	// RebuildAfter is how long an owner must be gone before a range with **no**
	// live follower is reassigned anyway. Long, and deliberately much longer
	// than PromoteAfter: there is nothing to promote, so the new owner starts
	// empty and the whole plane must be rebuilt from the disks before it can be
	// trusted again.
	//
	// A node rebooting must not cost that. Waiting leaves the range unserved,
	// which is an outage for part of the keyspace — but a bounded one that ends
	// when the node comes back, against a cluster-wide walk of every disk that
	// does not.
	//
	// Zero means DefaultRebuildAfter.
	RebuildAfter time.Duration

	// MaxChurn is the fraction of range owners that may be missing before the
	// controller refuses to act at all — see Controller.churn. A whole
	// membership vanishing is a control-plane read gone wrong far more often
	// than it is a cluster that has.
	//
	// Zero means DefaultMaxChurn. Values above 1 disable the ceiling.
	MaxChurn float64

	// Now is the clock, for tests. Zero means time.Now.
	Now func() time.Time
}

// Controller reconciles the partitioning with the membership.
//
// # What it does not do
//
// It does not rebalance. A range whose owner is alive is never moved, however
// lopsided the assignment — spreading load is a data move, and a controller
// that did it on every membership change would move ranges every time a node
// restarted. Splitting a hot range and placing a new node's share are separate
// decisions with their own costs, and neither belongs on the failure path.
//
// # One writer, but it does not depend on that
//
// A node runs this under the cluster-wide election, so ordinarily there is one.
// Elections have a window where two candidates both believe they hold it, and
// the reconciliation is deterministic for exactly that reason: two controllers
// reacting to one failure compute the same map, so the second write is a
// duplicate rather than a disagreement.
type Controller struct {
	cfg ControllerConfig

	// absent is when each missing node was first noticed missing. It is the
	// controller's only state, and it is intentionally in memory: a controller
	// that just took over has not watched anyone yet, so it starts everyone's
	// clock now. That delays a failover it could have made sooner, and never
	// causes one it should not have made — the safe direction, because the
	// mistake in the other direction is a rebuild nobody asked for.
	absent map[cluster.NodeID]time.Time
}

// Reconciliation is what one pass decided.
type Reconciliation struct {
	// Changed reports whether the partitioning was written.
	Changed bool
	// Promoted, Held and Orphaned are as on Reassignment.
	Promoted []rangemap.Range
	Held     []rangemap.Range
	Orphaned []rangemap.Range
	// Split are the boundaries this pass created, and which median chose each.
	Split []SplitPlan
	// Learned are the ranges whose learner finished its copy and became an
	// ordinary follower this pass.
	Learned []rangemap.Range
	// Moved are the ranges handed to their destination this pass — the last
	// edit of a move, after which the range is served by the node it moved to.
	Moved []rangemap.Range
	// Trimmed are the ranges dropped back to the configured replica count this
	// pass, after a move left them one copy over it.
	Trimmed []rangemap.Range
	// Started is the move this pass began, if any — the first of a move's three
	// edits, and the only one that decides anything.
	Started *Move
}

// RebuildOwed reports whether this pass left ranges that hold nothing, which
// the caller must rebuild before the plane can be trusted.
func (r Reconciliation) RebuildOwed() bool { return len(r.Orphaned) > 0 }

// NewController returns a controller. It performs no I/O until Reconcile.
func NewController(cfg ControllerConfig) (*Controller, error) {
	if cfg.Load == nil || cfg.Save == nil {
		return nil, errors.New("controller needs a way to read and write the partitioning")
	}

	if cfg.Live == nil {
		return nil, errors.New("controller needs a membership")
	}

	if cfg.PromoteAfter <= 0 {
		cfg.PromoteAfter = DefaultPromoteAfter
	}

	if cfg.RebuildAfter <= 0 {
		cfg.RebuildAfter = DefaultRebuildAfter
	}

	if cfg.MaxChurn <= 0 {
		cfg.MaxChurn = DefaultMaxChurn
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &Controller{cfg: cfg, absent: make(map[cluster.NodeID]time.Time)}, nil
}

// Reconcile makes one pass: notice who is gone, move what has been gone long
// enough, and write the result.
//
// Idempotent and cheap when nothing is wrong — a pass where every owner is
// alive reads the map and writes nothing.
func (c *Controller) Reconcile(ctx context.Context) (Reconciliation, error) {
	m, err := c.cfg.Load(ctx)
	if err != nil {
		return Reconciliation{}, errors.Wrap(err, "read the partitioning")
	}

	if m == nil || len(m.Ranges) == 0 {
		return Reconciliation{}, errors.New("the metadata plane is not partitioned")
	}

	live := c.cfg.Live()

	if err := c.churn(m, live); err != nil {
		return Reconciliation{}, err
	}

	now := c.cfg.Now()
	c.track(m, live, now)

	out, err := ReassignWith(m, live, func(r rangemap.Range, promotable bool) bool {
		return c.hold(r.Owner, promotable, now)
	})
	if err != nil {
		return Reconciliation{}, err
	}

	result := Reconciliation{
		Promoted: out.Promoted,
		Held:     out.Held,
		Orphaned: out.Orphaned,
	}

	if len(out.Promoted) == 0 && len(out.Orphaned) == 0 {
		// Nothing is wrong with the membership, so the pass can spend itself on
		// making the partitioning better rather than correct.
		//
		// One thing per pass, never two. A pass that failed over *and* split
		// would be deciding where data goes from measurements taken while it
		// was moving, and the second decision would be made against a map the
		// first had already invalidated. The same holds for a promotion: it
		// rewrites the map, and a split planned from the map before it would be
		// applied to one that no longer matches. Neither is urgent, and the next
		// pass is five seconds away.
		// Moves in flight are finished before any are started. A pass that
		// handed one range over and named a learner on another would be two
		// decisions again, and the one already paid for is the one to finish:
		// a range mid-move is holding an extra replica until it completes.
		result, moved, err := c.finish(ctx, m, result)
		if err != nil {
			return Reconciliation{}, err
		}

		if moved {
			return result, nil
		}

		result, trimmed, err := c.trim(ctx, m, result)
		if err != nil {
			return Reconciliation{}, err
		}

		if trimmed {
			return result, nil
		}

		result, promoted, err := c.promote(ctx, m, result)
		if err != nil {
			return Reconciliation{}, err
		}

		if promoted {
			return result, nil
		}

		return c.improve(ctx, m, result)
	}

	// Building before the map, not after. A node that picked up a map with an
	// orphaned range while the plane still read ready would serve that range as
	// authoritative, and an empty range answers "no such object" for objects
	// that exist — a wrong answer rather than a slow one.
	if len(out.Orphaned) > 0 && c.cfg.Readiness != nil {
		if err := c.cfg.Readiness.Set(ctx, metastore.Building(metastore.CauseOrphaned)); err != nil {
			return Reconciliation{}, errors.Wrap(err, "mark the plane building")
		}
	}

	if err := c.cfg.Save(ctx, out.Map); err != nil {
		return Reconciliation{}, errors.Wrap(err, "write the partitioning")
	}

	result.Changed = true

	return result, nil
}

// churn refuses a pass where too much of the membership is missing at once.
//
// A controller that cannot see most of the cluster is misreading the registry
// far more often than it is watching most of the cluster fail, and the two look
// identical from here. They do not deserve the same response: reassigning
// everything at once is useless in the second case — there is nowhere to put it
// — and catastrophic in the first, because it orphans ranges whose owners are
// fine and bills the cluster a full rebuild for a control-plane blip.
//
// So a pass that would move most of the plane does nothing and says why. If the
// failure is real, the ranges are unserved either way, and a human is better
// placed than a timer to decide what a cluster in that state should do.
func (c *Controller) churn(m *rangemap.Map, live []cluster.NodeID) error {
	owners := make(map[cluster.NodeID]bool, len(m.Ranges))

	for _, r := range m.Ranges {
		owners[r.Owner] = true
	}

	missing := 0

	for owner := range owners {
		if !slices.Contains(live, owner) {
			missing++
		}
	}

	// A share alone is the wrong test on a small plane, where one owner of two
	// going away is half the cluster and also just a node failing. Below the
	// floor the fraction carries no information, so only the absolute count
	// decides, and it decides to proceed.
	if missing < churnFloor {
		return nil
	}

	if float64(missing) > c.cfg.MaxChurn*float64(len(owners)) {
		return errors.Errorf(
			"%d of %d range owners are missing, over the %.0f%% ceiling: refusing to reassign the plane",
			missing, len(owners), c.cfg.MaxChurn*100)
	}

	return nil
}

// finish hands over the ranges whose destination has become a follower.
//
// The last edit of a move, and the one that actually changes who serves the
// range. It is cheap by construction: the destination already holds everything
// the owner held, which is what being promoted out of a learner established.
//
// # Nothing is re-decided here
//
// Whether the range should move was settled when the move started, and this pass
// does not revisit it. A controller that re-derived the decision from load would
// abandon moves whose justification had shifted while they ran — leaving the
// range with a permanent extra replica and no move, which is the worst of both.
//
// # A destination that is gone is waited for, not replaced
//
// It holds a full copy of the range, and the range is still being served by its
// current owner. So a move whose destination went away costs nothing while it
// waits, and the alternative — picking a different destination — throws away a
// finished copy to start another.
//
// # A missing owner does not hold this up
//
// Like promotion and splitting, this runs on a pass that did not reassign
// anything. Unlike them, a range merely *held* — an owner gone but still inside
// its grace — is no reason to wait: the destination already holds everything the
// owner had, so handing it over is the one edit that resolves the absence
// instead of enduring it. A move that happened to be one pass from done finishes
// rather than waiting out a grace it does not need.
func (c *Controller) finish(
	ctx context.Context,
	m *rangemap.Map,
	result Reconciliation,
) (Reconciliation, bool, error) {
	live := c.cfg.Live()
	next := m

	for i, r := range m.Ranges {
		// Still a learner, so the copy is not done and Handover would refuse.
		// Checked here so the refusal is a state this pass understands rather
		// than an error it has to interpret.
		if r.MoveTo == "" || !slices.Contains(r.Followers, r.MoveTo) {
			continue
		}

		if !slices.Contains(live, r.MoveTo) {
			continue
		}

		// Applied one at a time onto the result of the last, because each
		// returns a new map and the next handover must be made against it.
		// Handover replaces one range in place, so the index still holds.
		handed, err := next.Handover(r.Start)
		if err != nil {
			// A range this pass cannot hand over is left for the next one. The
			// map is written from whatever did succeed, so one stuck move does
			// not hold up the others.
			continue
		}

		next = handed

		result.Moved = append(result.Moved, next.Ranges[i])
	}

	if len(result.Moved) == 0 {
		return result, false, nil
	}

	if err := c.cfg.Save(ctx, next); err != nil {
		return result, false, errors.Wrap(err, "write the partitioning")
	}

	result.Changed = true

	return result, true, nil
}

// trim drops ranges back to the configured replica count.
//
// # A completed move leaves one copy too many, by design
//
// Handover gives the range to its destination and keeps the node it replaced as
// a follower, because dropping it in the same edit would take the replica count
// down at the moment ownership changes — a rebalance deciding durability, which
// nobody asked it to. But the destination was added as a replica when the move
// started, so the range comes out of a move holding one more copy than it went
// in with. Something has to give it back, and this is that something: a separate
// edit, on a later pass, when the range is settled.
//
// # The old owner is the one dropped
//
// Surplus followers are taken from the end of the list, which is where Handover
// appends the node it replaced — and that is the right one on the merits, not
// merely by position. A range is moved off a node to relieve it; leaving a
// replica behind means its disk still holds the range, so the move relieved
// nothing it was meant to.
//
// # Never below the target, and never on a range mid-move
//
// A range that is still moving is *supposed* to have an extra copy — that is
// what the copy is. Trimming it would take away the replica the move is in the
// middle of making.
func (c *Controller) trim(
	ctx context.Context,
	m *rangemap.Map,
	result Reconciliation,
) (Reconciliation, bool, error) {
	if c.cfg.Replicas < 1 {
		return result, false, nil
	}

	next := &rangemap.Map{Revision: m.Revision, Ranges: slices.Clone(m.Ranges)}
	changed := false

	for i, r := range next.Ranges {
		// The owner counts as one, so the followers wanted are one fewer.
		want := c.cfg.Replicas - 1
		if r.MoveTo != "" || len(r.Followers) <= want {
			continue
		}

		trimmed := r
		trimmed.Followers = slices.Clone(r.Followers[:want])
		next.Ranges[i] = trimmed

		changed = true

		result.Trimmed = append(result.Trimmed, trimmed)
	}

	if !changed {
		return result, false, nil
	}

	if err := next.Validate(); err != nil {
		return result, false, errors.Wrap(err, "trim produced an invalid map")
	}

	if err := c.cfg.Save(ctx, next); err != nil {
		return result, false, errors.Wrap(err, "write the partitioning")
	}

	result.Changed = true

	return result, true, nil
}

// promote turns learners that have finished their copy into ordinary followers.
//
// # Into Followers, never straight to Owner
//
// A move is a promotion in slow motion, and this is the step that ends the slow
// part. What it grants is exactly what a learner lacked: the right to be
// promoted. Handing over ownership in the same edit would make one decision out
// of two — "this node holds the range" and "this node should serve it" — and
// only the first has been established here. Ownership moves afterwards, by the
// path failover already uses, so a promotion made on a wrong answer costs a
// stale replica rather than a range served with a hole in it.
//
// # A learner is asked, not measured
//
// Only the destination knows what landed. A learner that cannot be reached, or
// that has not finished, is left exactly as it is — a learner. That is the safe
// standstill: the range keeps being served by its owner, and nothing about the
// plane degrades while a move waits.
//
// Runs only on a pass where the membership needed nothing, for the same reason
// splitting does: a promotion is a rewrite of the map, and one made from a map
// that failover has just invalidated would be describing a cluster that no
// longer exists.
func (c *Controller) promote(
	ctx context.Context,
	m *rangemap.Map,
	result Reconciliation,
) (Reconciliation, bool, error) {
	if c.cfg.Ready == nil {
		return result, false, nil
	}

	live := c.cfg.Live()

	next := &rangemap.Map{Revision: m.Revision, Ranges: slices.Clone(m.Ranges)}
	changed := false

	for i, r := range next.Ranges {
		for _, learner := range r.Learners {
			// A learner that is gone is not asked. Its absence is the
			// membership's business, and this pass runs only when the
			// membership needed nothing — so a missing learner here is one
			// whose range still has a live owner and nothing to fix.
			if !slices.Contains(live, learner) {
				continue
			}

			ready, err := c.cfg.Ready(ctx, learner, r)
			if err != nil || !ready {
				// Unreachable and unfinished are the same answer. Neither is a
				// reason to fail the pass: the move waits, which costs nothing
				// but time, and the alternative is a controller that stops
				// reconciling because one node is slow.
				continue
			}

			promoted := next.Ranges[i]
			promoted.Learners = withoutNode(promoted.Learners, learner)
			promoted.Followers = append(slices.Clone(promoted.Followers), learner)
			next.Ranges[i] = promoted

			changed = true

			result.Learned = append(result.Learned, promoted)
		}
	}

	if !changed {
		return result, false, nil
	}

	if err := next.Validate(); err != nil {
		return result, false, errors.Wrap(err, "promotion produced an invalid map")
	}

	if err := c.cfg.Save(ctx, next); err != nil {
		return result, false, errors.Wrap(err, "write the partitioning")
	}

	result.Changed = true

	return result, true, nil
}

// improve makes the partitioning better rather than merely correct: finer where
// the data warrants it, and more evenly placed where the load does.
//
// Runs only on a pass where the membership needed nothing, so every range has a
// live owner to be measured — a range whose owner is gone cannot be measured,
// and deciding from the last number anyone saw would be a map edit made from a
// stale reading.
//
// # Split before move, and never both
//
// A split costs nothing: both halves are already on the right side of the new
// boundary, so it is a map edit and no I/O at all. A move is a full copy. Doing
// the free thing first is not merely cheaper — it is what makes the expensive
// thing possible, because a node whose load sits in one enormous range has no
// range small enough to place, and splitting is what produces the halves that
// fit.
//
// So a pass that splits does not also move. The measurements it moved by are
// about ranges that no longer exist.
func (c *Controller) improve(
	ctx context.Context,
	m *rangemap.Map,
	result Reconciliation,
) (Reconciliation, error) {
	if c.cfg.Measure == nil {
		return result, nil
	}

	// One survey, both planners. Asking every owner twice a pass would double
	// the traffic on the path that runs every five seconds, and could decide
	// two things from two different readings of one cluster.
	survey := c.survey(ctx, m)

	boundaries := PlanSplits(m, survey, c.cfg.Split)
	if len(boundaries) == 0 {
		return c.startMove(ctx, m, survey, result)
	}

	next := m

	for _, plan := range boundaries {
		// Applied one at a time onto the result of the last, because two
		// boundaries can land in the same range: the plan is computed from one
		// map, and the first split of a range invalidates the second's view of
		// it. Split rejects what it cannot apply, and a rejected boundary is
		// skipped rather than fatal — the range is measured again next pass.
		split, err := next.Split(plan.At)
		if err != nil {
			continue
		}

		next = split

		result.Split = append(result.Split, plan)
	}

	if len(result.Split) == 0 {
		return c.startMove(ctx, m, survey, result)
	}

	if err := c.cfg.Save(ctx, next); err != nil {
		return Reconciliation{}, errors.Wrap(err, "write the partitioning")
	}

	result.Changed = true

	return result, nil
}

// survey asks every range's owner what it holds and what it is taking.
//
// One entry per range, in map order, nil where the owner could not be reached —
// which the planners read as "no answer" rather than as a zero. A node that
// cannot be asked is not a node that is idle, and the difference decides whether
// it is chosen as somewhere to put more work.
func (c *Controller) survey(ctx context.Context, m *rangemap.Map) Survey {
	out := make(Survey, len(m.Ranges))

	for i, r := range m.Ranges {
		if err := ctx.Err(); err != nil {
			return out
		}

		got, err := c.cfg.Measure(ctx, r.Owner, r)
		if err != nil {
			continue
		}

		out[i] = &got
	}

	return out
}

// startMove begins one move, if the load is uneven enough to be worth a copy.
//
// The first of a move's three edits and the only one that decides anything —
// everything after it carries out what this wrote. See PlanRebalance for what
// makes a move worth starting and why only one runs at a time.
func (c *Controller) startMove(
	ctx context.Context,
	m *rangemap.Map,
	survey Survey,
	result Reconciliation,
) (Reconciliation, error) {
	move, ok := PlanRebalance(m, survey, c.cfg.Live(), c.cfg.Rebalance)
	if !ok {
		return result, nil
	}

	next, err := m.StartMove(move.At, move.To)
	if err != nil {
		// A move the map will not accept is one the next pass plans again from
		// a fresher picture. Nothing is written, so nothing is half-started.
		return result, nil
	}

	if err := c.cfg.Save(ctx, next); err != nil {
		return Reconciliation{}, errors.Wrap(err, "write the partitioning")
	}

	result.Changed = true
	result.Started = &move

	return result, nil
}

// track records when each missing node was first noticed missing.
//
// Rebuilt each pass rather than mutated, which is what makes both of the cases
// that matter fall out without a rule of their own: a node that came back is
// simply not in the new map, so its clock is gone, and a node that no longer
// owns anything does not accumulate. Each absence therefore starts its own
// clock — a node gone for twenty seconds twice is never treated as gone for
// forty, which is how a rolling restart would otherwise become a plane-wide
// rebuild.
func (c *Controller) track(m *rangemap.Map, live []cluster.NodeID, now time.Time) {
	next := make(map[cluster.NodeID]time.Time, len(c.absent))

	for _, r := range m.Ranges {
		if slices.Contains(live, r.Owner) {
			continue
		}

		if since, ok := c.absent[r.Owner]; ok {
			next[r.Owner] = since
		} else {
			next[r.Owner] = now
		}
	}

	c.absent = next
}

// hold reports whether a range whose owner is missing should be left alone for
// now, which is true until that owner has been gone past the grace its range
// deserves — short if there is something to promote, long if there is not.
func (c *Controller) hold(owner cluster.NodeID, promotable bool, now time.Time) bool {
	since, ok := c.absent[owner]
	if !ok {
		// Not tracked, so not yet noticed missing. Hold: the very first pass
		// after a controller takes over must not act on an absence it has not
		// timed.
		return true
	}

	grace := c.cfg.RebuildAfter
	if promotable {
		grace = c.cfg.PromoteAfter
	}

	return now.Sub(since) < grace
}
