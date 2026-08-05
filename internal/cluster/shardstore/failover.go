package shardstore

import (
	"slices"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// Reassignment is what a failover decided.
type Reassignment struct {
	// Map is the partitioning after the decision. Always a valid partition,
	// because a range with nowhere to go is still assigned — see Orphaned.
	Map *rangemap.Map
	// Promoted are the ranges whose owner was gone and whose follower took
	// over. These are cheap: the follower already holds what the owner held.
	Promoted []rangemap.Range
	// Held are the ranges whose owner was gone but which a caller's veto kept
	// where they are — see ReassignWith. Nobody serves them until the owner
	// returns or the hold lifts, which for the no-follower case is the cheaper
	// of the two bad options.
	Held []rangemap.Range
	// Orphaned are the ranges whose owner was gone with no live follower to
	// promote. They have been assigned to a node anyway — a range with no owner
	// is a key nothing serves — but that node holds **nothing** for them.
	//
	// This is the expensive case and the one a caller must act on: the plane
	// cannot be trusted until they are rebuilt, because a node serving an empty
	// range answers "no such object" for objects that exist. See the rebuild
	// note on Reassign.
	Orphaned []rangemap.Range
}

// Reassign moves ranges off nodes that are gone.
//
// # What it decides, and what it cannot
//
// A range whose owner is live is untouched — this is failover, not rebalancing,
// and moving a healthy range would cost a data move for no reason.
//
// A range whose owner is gone goes to a live follower. Which one is decided by
// follower order, and that is a real limitation rather than a choice: currency
// is what should decide it, and nothing here knows how far behind each follower
// is. Promoting a stale follower costs the plane the entries it missed, and
// nothing repairs those short of a rebuild — so follower lag is the input this
// wants as soon as it exists, and until then the ordering is the only signal.
//
// A range whose owner is gone with no live follower is **orphaned**. It is
// still assigned, to the live node holding the fewest ranges, because a range
// with no owner is a key nothing serves and that is worse. But the node holds
// nothing for it.
//
// # Orphans make the plane untrustworthy until rebuilt
//
// A node serving an empty range answers "no such object" for objects that
// exist, which is a wrong answer rather than a slow one. So a caller that sees
// Orphaned must mark the plane building before applying the map — listings then
// fall back to the sidecar walk, which is slower and always right — and run the
// cluster-wide rebuild.
//
// That is the promote-or-rebuild split: promotion is a metadata change, rebuild
// is a cluster walk, and which one a failure costs is decided entirely by
// whether a follower was there.
func Reassign(m *rangemap.Map, live []cluster.NodeID) (Reassignment, error) {
	return ReassignWith(m, live, nil)
}

// ReassignWith is Reassign with a veto: hold reports the ranges to leave where
// they are even though their owner is gone.
//
// It exists because "the owner is gone" is not one question. A range with a
// live follower fails over for the cost of a metadata write; a range without
// one costs a cluster-wide walk of every disk. Those two do not deserve the
// same patience, and nothing here knows how long anyone has been gone — that
// is the caller's to decide, and the Controller's grace periods are what it
// decides it with.
//
// A held range is not served by anyone until its owner returns or the hold
// lifts. That is the price, and it is deliberate: for the expensive case,
// unavailable-for-now beats rebuilt-because-a-node-rebooted.
func ReassignWith(
	m *rangemap.Map,
	live []cluster.NodeID,
	hold func(r rangemap.Range, promotable bool) bool,
) (Reassignment, error) {
	if m == nil {
		return Reassignment{}, errors.New("no range map to reassign")
	}

	if len(live) == 0 {
		return Reassignment{}, errors.New("no live nodes to assign ranges to")
	}

	if err := m.Validate(); err != nil {
		return Reassignment{}, errors.Wrap(err, "refusing to reassign an invalid map")
	}

	out := Reassignment{Map: &rangemap.Map{
		Revision: m.Revision,
		Ranges:   make([]rangemap.Range, 0, len(m.Ranges)),
	}}

	// Load is counted as the decision proceeds, so orphans spread across nodes
	// rather than piling onto whichever happened to be least loaded first.
	load := make(map[cluster.NodeID]int, len(live))

	for _, r := range m.Ranges {
		if slices.Contains(live, r.Owner) {
			load[r.Owner]++
		}
	}

	for _, r := range m.Ranges {
		if slices.Contains(live, r.Owner) {
			out.Map.Ranges = append(out.Map.Ranges, r)

			continue
		}

		// Followers alone, never Learners. A learner is mid-backfill: it holds
		// some of the range, and a promoted learner answers "no such object"
		// for every key the backfill has not reached — which nothing would
		// report, because a partial range is a range that simply says no.
		promoted, promotable := firstLive(r.Followers, live)

		if hold != nil && hold(r, promotable) {
			out.Map.Ranges = append(out.Map.Ranges, r)
			out.Held = append(out.Held, r)

			continue
		}

		next := r

		if promotable {
			next.Owner = promoted
			// The promoted node is no longer one of its own followers. Restoring
			// R is re-replication — a data move — and deliberately not decided
			// here, where nothing is moved.
			next.Followers = withoutNode(r.Followers, promoted)
			next.Learners = withoutNode(r.Learners, promoted)

			load[promoted]++

			out.Promoted = append(out.Promoted, next)
		} else {
			next.Owner = leastLoaded(live, load)
			next.Followers = withoutNode(r.Followers, next.Owner)
			// A learner handed the range as an orphan is now its owner, and an
			// owner learning from itself is the one shape Validate refuses.
			// What it holds is still partial — that is what Orphaned means.
			next.Learners = withoutNode(r.Learners, next.Owner)

			load[next.Owner]++

			out.Orphaned = append(out.Orphaned, next)
		}

		// A failover that hands the range to the node it was already moving to
		// has completed that move by another route. Left set, the intent would
		// name the owner as its own destination, which Validate refuses — and
		// rightly: there is nothing left to hand over.
		if next.Owner == next.MoveTo {
			next.MoveTo = ""
		}

		out.Map.Ranges = append(out.Map.Ranges, next)
	}

	if err := out.Map.Validate(); err != nil {
		return Reassignment{}, errors.Wrap(err, "reassignment produced an invalid map")
	}

	return out, nil
}

// firstLive returns the first follower that is still up.
//
// Order is the only currency signal available; see the note on Reassign.
func firstLive(followers, live []cluster.NodeID) (cluster.NodeID, bool) {
	for _, f := range followers {
		if slices.Contains(live, f) {
			return f, true
		}
	}

	return "", false
}

// withoutNode drops a node from a follower list, returning nil rather than an
// empty slice when nothing is left — so a range with no followers compares
// equal to one that never had any.
func withoutNode(followers []cluster.NodeID, node cluster.NodeID) []cluster.NodeID {
	var out []cluster.NodeID

	for _, f := range followers {
		if f != node {
			out = append(out, f)
		}
	}

	return out
}

// leastLoaded returns the live node owning the fewest ranges, breaking ties by
// name so the decision is the same whoever computes it.
//
// Determinism matters more than it looks: two nodes racing to reassign the same
// failure must not produce different maps, or the fenced write that decides
// between them would be settling a disagreement rather than a duplicate.
func leastLoaded(live []cluster.NodeID, load map[cluster.NodeID]int) cluster.NodeID {
	ordered := slices.Clone(live)
	slices.Sort(ordered)

	best := ordered[0]
	for _, node := range ordered[1:] {
		if load[node] < load[best] {
			best = node
		}
	}

	return best
}
