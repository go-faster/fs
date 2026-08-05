package shardstore

import (
	"context"
	"slices"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// CatchUpResult is what one catch-up pass did.
type CatchUpResult struct {
	// Learning is how many ranges the map names this node a learner of.
	Learning int
	// Copied are the ranges this pass finished copying. A range appears here
	// once — the pass that completes it — and in Ready from then on.
	Copied []rangemap.Range
	// Ready are the ranges this node has finished learning: it holds what the
	// owner held, the log is keeping it current, and it is waiting to be
	// promoted. This is the signal a promotion is decided on.
	Ready []rangemap.Range
	// Failed are the ranges this pass could not finish. Not fatal and not
	// remembered: an owner that could not be reached is the ordinary case, and
	// the next pass resumes from the cursor rather than from the beginning.
	Failed []rangemap.Range
	// Entries is how many records this pass copied.
	Entries int
}

// CatchUp copies every range this node is learning from the node that owns it.
//
// # Unelected, and it has to be
//
// Every other decision about the partitioning is made by one elected controller,
// because the map has one writer. This is not a decision about the map at all:
// the controller has already written that this node is learning a range, and
// this is that node doing what it was named for. Only the node being copied into
// can do it, so every node runs its own — and a node that is a learner of
// nothing does nothing, which is almost every node almost always.
//
// # A finished range is remembered, in memory
//
// A learner stays a learner until the controller promotes it, so without this a
// pass would re-read the whole range from its owner every tick. The cursor
// cannot serve here: it is cleared on completion precisely so that no durable
// claim of doneness outlives the data behind it.
//
// So doneness is process-local, like the controller's absence clock and for the
// same reason: a node that has just started has copied nothing in this process's
// lifetime, and re-copying a range it had already finished costs one pass and
// nothing else. The mistake in the other direction — trusting a remembered
// completion across a restart — is a learner promoted onto data it does not
// have.
func (p *Plane) CatchUp(ctx context.Context) (CatchUpResult, error) {
	// Freshly loaded rather than taken from the routing cache, and this is the
	// one place the plane reads the map on a timer rather than on demand.
	//
	// Routing deliberately does not poll: a stale route costs one refused round
	// trip and corrects itself, so the map is read when a peer says it is behind
	// and not otherwise. That works because the signal rides on traffic that was
	// happening anyway. A move has no such traffic — a node that has just been
	// named a learner is not being routed to, by definition, and the owner's log
	// batches are refused until it finds out. Left to the routing cache, a move
	// would wait for a request that may never come.
	//
	// So this pays one small read per node per pass to make a move progress
	// without depending on load. It also reconfigures the shard, which is what
	// makes the learner start accepting the owner's log rather than refusing it.
	m, err := p.Refresh(ctx)
	if err != nil {
		return CatchUpResult{}, errors.Wrap(err, "read the partitioning")
	}

	if m == nil {
		return CatchUpResult{}, nil
	}

	var out CatchUpResult

	for _, r := range m.Ranges {
		if !slices.Contains(r.Learners, p.self) {
			continue
		}

		out.Learning++

		// The completion lives on the shard, which is where the copy landed and
		// what a peer can be asked about. Refresh above has already pruned it to
		// the ranges this node is still learning, so a claim from before a
		// promotion cannot be read here.
		if p.shard.CaughtUp(r) {
			out.Ready = append(out.Ready, r)

			continue
		}

		res, err := p.backfill(ctx, r)
		if err != nil {
			// Recorded and left for the next pass. A backfill that could not
			// finish has still made progress, and its cursor says how much.
			out.Failed = append(out.Failed, r)

			continue
		}

		out.Entries += res.Entries

		if res.Done {
			out.Copied = append(out.Copied, r)
			out.Ready = append(out.Ready, r)
		}
	}

	return out, nil
}

// backfill copies one range from its owner into this node's shard.
func (p *Plane) backfill(ctx context.Context, r rangemap.Range) (BackfillResult, error) {
	backend, err := p.resolve(ctx, r.Owner)
	if err != nil {
		return BackfillResult{}, err
	}

	from, ok := backend.(Source)
	if !ok {
		// A peer that cannot be read out of is one running a binary without the
		// operation. Reported rather than skipped: the move is stuck until it is
		// upgraded, and a silent skip would look like a move that is merely slow.
		return BackfillResult{}, errors.Errorf("node %s cannot be read as a backfill source", r.Owner)
	}

	return p.shard.Backfill(ctx, r, from, DefaultBackfillBatch)
}
