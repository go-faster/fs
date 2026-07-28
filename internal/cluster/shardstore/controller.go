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
	// Split are the boundaries this pass created.
	Split []string
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
		// One or the other, never both. A pass that failed over *and* split
		// would be deciding where data goes from measurements taken while it
		// was moving, and the second decision would be made against a map the
		// first had already invalidated. Splits wait a pass; they are not
		// urgent, and the next pass is five seconds away.
		return c.split(ctx, m, result)
	}

	// Building before the map, not after. A node that picked up a map with an
	// orphaned range while the plane still read ready would serve that range as
	// authoritative, and an empty range answers "no such object" for objects
	// that exist — a wrong answer rather than a slow one.
	if len(out.Orphaned) > 0 && c.cfg.Readiness != nil {
		if err := c.cfg.Readiness.Set(ctx, metastore.StateBuilding); err != nil {
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

// split makes the partitioning finer where the data warrants it.
//
// Runs only on a pass where the membership needed nothing, so every range has a
// live owner to be measured — a range whose owner is gone cannot be measured,
// and splitting it on the last number anyone saw would be a map edit made from
// a stale reading.
func (c *Controller) split(
	ctx context.Context,
	m *rangemap.Map,
	result Reconciliation,
) (Reconciliation, error) {
	if c.cfg.Measure == nil {
		return result, nil
	}

	boundaries := PlanSplits(ctx, m, c.cfg.Measure, c.cfg.Split)
	if len(boundaries) == 0 {
		return result, nil
	}

	next := m

	for _, at := range boundaries {
		// Applied one at a time onto the result of the last, because two
		// boundaries can land in the same range: the plan is computed from one
		// map, and the first split of a range invalidates the second's view of
		// it. Split rejects what it cannot apply, and a rejected boundary is
		// skipped rather than fatal — the range is measured again next pass.
		split, err := next.Split(at)
		if err != nil {
			continue
		}

		next = split

		result.Split = append(result.Split, at)
	}

	if len(result.Split) == 0 {
		return result, nil
	}

	if err := c.cfg.Save(ctx, next); err != nil {
		return Reconciliation{}, errors.Wrap(err, "write the partitioning")
	}

	result.Changed = true

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
