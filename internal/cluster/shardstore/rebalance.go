package shardstore

import (
	"cmp"
	"slices"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// Survey is what one pass measured: one entry per range, in map order, and nil
// where the owner could not be reached.
//
// Taken once and shared by every planner that wants it. Splitting and
// rebalancing ask the same owners the same question, and a pass that asked twice
// would double the control-plane traffic on the path that runs every five
// seconds — and could decide two things from two different readings of one
// cluster.
type Survey []*Measurement

// RebalancePolicy is when a range is worth moving to relieve its owner.
type RebalancePolicy struct {
	// MinGap is how much busier, in writes a second, the busiest node must be
	// than the quietest before anything moves. Zero means DefaultRebalanceGap.
	//
	// A floor rather than a ratio. Ratios misbehave exactly where this is used:
	// a quiet node makes any gap look infinite, so a cluster doing almost
	// nothing would shuffle ranges over a handful of writes.
	MinGap float64
}

// DefaultRebalanceGap is the write-rate difference below which a cluster is
// balanced enough.
//
// A move is a full copy of a range, so the gap has to be worth paying for.
// Below a few tens of writes a second the difference between two nodes is
// noise, sampling, or a burst that will be gone before the copy finishes — and
// a rebalance that chased it would spend the cluster's bandwidth on churn.
const DefaultRebalanceGap = 25

func (p RebalancePolicy) minGap() float64 {
	if p.MinGap <= 0 {
		return DefaultRebalanceGap
	}

	return p.MinGap
}

// Move is a range worth handing to another node.
type Move struct {
	// At is a key inside the range to move — what StartMove names it by.
	At string
	// To is the node to hand it to.
	To cluster.NodeID
	// Gap is the write-rate difference the move is closing, and Writes what the
	// range itself is taking. For logging: a move that cannot be explained is
	// one an operator has to reverse-engineer from the map.
	Gap    float64
	Writes float64
}

// PlanRebalance picks one range to move from the busiest node to the quietest,
// or reports that the cluster is balanced enough to leave alone.
//
// # Load, not bytes
//
// Size says what a range costs to move; it says nothing about what is gained.
// For the workload this exists for the two point opposite ways — sequential keys
// all land at the top of the key space, so the busiest range is the newest and
// the smallest — and a rebalance reading bytes would move everything except the
// range taking the writes.
//
// Capacity here is capacity to take load. Disk capacity is a different question
// with a different answer, and it belongs where disks are known rather than in
// the partitioning.
//
// # One at a time
//
// A move is a full copy of a range. Starting several at once would have the
// cluster spend its bandwidth on rebalancing rather than on serving, and each
// one is decided from a picture of the load that the others are in the middle of
// changing. So a pass that finds a move already in flight plans nothing.
//
// # The move must not reverse the imbalance
//
// A range is moved only when it is at most half the gap it is closing. Moving a
// larger one would leave the destination busier than the source was, and the
// next pass would move it back — a cluster ping-ponging one range forever, doing
// full copies each way.
//
// That refusal is also where split and move compose, and why #194 says neither
// converges alone. A node whose load is one enormous range has no range small
// enough to move: the gap is the range. Splitting is what produces the halves
// that fit, and a split costs nothing because it moves nothing — so the sequence
// is split until a piece fits, then move it.
//
// # Deterministic
//
// Two controllers racing one pass must plan the same move from the same survey,
// so every choice is ordered and ties break on node and key.
func PlanRebalance(
	m *rangemap.Map,
	survey Survey,
	live []cluster.NodeID,
	policy RebalancePolicy,
) (Move, bool) {
	if len(survey) != len(m.Ranges) {
		return Move{}, false
	}

	for _, r := range m.Ranges {
		if r.MoveTo != "" {
			return Move{}, false
		}
	}

	load := make(map[cluster.NodeID]float64, len(live))

	// Every live node counts, including the ones owning nothing — a node that
	// has just joined is the emptiest destination there is, and one that only
	// appeared in the map would never be given anything.
	for _, node := range live {
		load[node] = 0
	}

	for i, r := range m.Ranges {
		if survey[i] == nil {
			// A range nobody could measure is a range whose owner's load is
			// unknown. Counting it as zero would make an unreachable node look
			// idle and elect it as the destination.
			return Move{}, false
		}

		if _, ok := load[r.Owner]; !ok {
			// An owner outside the membership. The pass that reassigns it is
			// not this one, and planning around a node that is gone would move
			// a range on behalf of a failure the controller has not handled yet.
			return Move{}, false
		}

		load[r.Owner] += survey[i].Writes
	}

	busiest, quietest, ok := ends(load)
	if !ok {
		return Move{}, false
	}

	gap := load[busiest] - load[quietest]
	if gap < policy.minGap() {
		return Move{}, false
	}

	// Half the gap: anything larger overshoots and comes straight back.
	limit := gap / 2

	best := -1

	for i, r := range m.Ranges {
		if r.Owner != busiest || survey[i].Writes > limit {
			continue
		}

		if best < 0 || survey[i].Writes > survey[best].Writes {
			best = i
		}
	}

	if best < 0 {
		// Every range on the busiest node would overshoot, which means its load
		// is concentrated in ranges too big to place. Splitting is what makes
		// them placeable, and it runs on the passes this one declines.
		return Move{}, false
	}

	return Move{
		At:     m.Ranges[best].Start,
		To:     quietest,
		Gap:    gap,
		Writes: survey[best].Writes,
	}, true
}

// ends returns the busiest and quietest nodes, breaking ties on the node ID so
// two controllers racing one pass agree.
func ends(load map[cluster.NodeID]float64) (busiest, quietest cluster.NodeID, ok bool) {
	if len(load) < 2 {
		// One node has nowhere to move anything to.
		return "", "", false
	}

	nodes := make([]cluster.NodeID, 0, len(load))
	for node := range load {
		nodes = append(nodes, node)
	}

	slices.SortFunc(nodes, func(a, b cluster.NodeID) int {
		if d := cmp.Compare(load[b], load[a]); d != 0 {
			return d
		}

		return cmp.Compare(a, b)
	})

	return nodes[0], nodes[len(nodes)-1], true
}
