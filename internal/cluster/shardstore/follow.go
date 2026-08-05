package shardstore

import (
	"context"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// ErrNotFollowed reports that a batch names a range this shard does not follow.
//
// Distinct from ErrNotOwned because the two are different mistakes. A caller
// that reaches the wrong owner has a stale map and should refetch; a batch
// arriving for a range this node does not follow means the *sender* is working
// from a stale follower set, and applying it would leave this shard holding
// data nothing will ever ask it for and nothing will clean up.
var ErrNotFollowed = errors.New("range is not followed by this shard")

// ErrNotLearned reports that a range is not one this shard is being copied into.
//
// Narrower than ErrNotFollowed and a different mistake again. A follower is
// current from the log and is the destination of no move, so a backfill aimed at
// one is a sender working from a map where this node is something it is not —
// and the entries would land on a node that will never be asked to hold them.
var ErrNotLearned = errors.New("range is not being learned by this shard")

// Shipper sends a batch an owner applied to the range's followers.
//
// It is called after the write is durable on the owner, from the write path,
// and it must ship batches for one range **in order** — pebble applies a batch
// as recorded, so a reordered pair leaves a follower with an older value where
// a newer one belongs. Ordering across different ranges does not matter, since
// no batch touches two.
//
// Failures are the shipper's to absorb. Replication is best effort by design:
// a batch that never lands costs the follower its currency, which costs a
// failover a rebuild — never an object, because the sidecars committed before
// any of this ran.
type Shipper func(ctx context.Context, r rangemap.Range, repr []byte)

// WithShipper makes this shard ship what it applies to its ranges' followers.
func WithShipper(fn Shipper) ShardOption {
	return func(s *Shard) { s.ship = fn }
}

// SetShipper installs the shipper after construction.
//
// Needed because shipping requires a way to reach other nodes, which requires
// the map, which the shard knows nothing about — so the wiring that has both
// cannot exist until after the shard does. Called once, before the shard takes
// writes; guarded anyway, because "before any writes" is an assumption about a
// caller rather than something enforced here.
func (s *Shard) SetShipper(fn Shipper) {
	s.mu.Lock()
	s.ship = fn
	s.mu.Unlock()
}

// Follow sets the ranges this shard replicates for another node.
//
// Separate from Adopt, and the distinction is load-bearing: a followed range is
// held but **not served**. A follower answering reads would be serving data it
// has no way to know is current, and the whole reason the owner is the only
// reader is that it is the only node that knows what it has applied.
func (s *Shard) Follow(ranges []rangemap.Range) {
	followed := make([]rangemap.Range, len(ranges))
	copy(followed, ranges)

	s.mu.Lock()
	s.followed = followed
	s.mu.Unlock()
}

// Configure sets what this shard serves, what it follows and what it is
// learning, together.
//
// Together rather than as an Adopt followed by a Follow, because between those
// two the shard would be running on half of one map and half of another. The
// half that matters is promotion: a node that has just taken over a range would
// briefly both own it and follow it, and the deposed owner is exactly the node
// most likely to still be shipping batches for it.
//
// # Followed and learned are separate here, not just in the map
//
// The shard used to be told only "these are the ranges you replicate", with the
// difference between a follower and a learner living entirely in rangemap. That
// made the shard unable to answer the one question a move turns on — *am I still
// being copied into?* — and it made a follower accept backfilled entries, which
// is data it was never told to receive.
//
// Keeping the two apart also gives the caught-up record its lifetime for free: a
// completion is remembered only while the range is still being learned, so a
// node promoted out of a range and later made a learner of it again cannot
// inherit a claim from before.
func (s *Shard) Configure(owned, followed, learned []rangemap.Range) {
	o := slices.Clone(owned)
	f := slices.Clone(followed)
	l := slices.Clone(learned)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.owned, s.followed, s.learned = o, f, l
	s.retainCaughtUp(l)
	s.trackLoad(o)
}

// Following returns the ranges this shard replicates for another node — as a
// follower or as a learner — in key order.
//
// Both, because replicating is what they have in common: each is a range this
// node holds and does not serve. What separates them is promotion, and the
// paths that care ask about that directly.
func (s *Shard) Following() []rangemap.Range {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]rangemap.Range, 0, len(s.followed)+len(s.learned))
	out = append(out, s.followed...)

	return append(out, s.learned...)
}

// replicates reports whether this shard may replay a batch for a range: it
// follows or learns it, and it is not one this shard owns.
//
// The second half is the split-brain guard, and it is the case that actually
// happens. A node promoted into a range is the authority for it; the node it
// was promoted over is the one most likely to still be shipping batches for
// that range, working from the map it had before it went away. Applying one
// would write a deposed owner's state into a range this node is answering for
// — the only way this plane could serve something no owner ever committed.
//
// Owning wins over following whatever the map says, because a map that lists a
// node as both is itself the mistake.
func (s *Shard) replicates(r rangemap.Range) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.servesLocked(r) {
		return false
	}

	return covers(s.followed, r) || covers(s.learned, r)
}

// learns reports whether this shard is being copied into for a range: it is in
// the learned set, and not one this shard owns.
//
// Narrower than replicates on purpose. A follower is kept current by the log and
// is not the destination of any move, so backfilled entries arriving for one are
// either a bug or a sender working from a map where this node is something it is
// not — and storing them would leave data on a node that will never be asked to
// hold it.
func (s *Shard) learns(r rangemap.Range) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.servesLocked(r) {
		return false
	}

	return covers(s.learned, r)
}

// servesLocked reports whether this shard was told to serve exactly this range.
// The caller holds the lock.
func (s *Shard) servesLocked(r rangemap.Range) bool {
	return covers(s.owned, r)
}

// covers reports whether a list names exactly this range.
func covers(ranges []rangemap.Range, r rangemap.Range) bool {
	for _, c := range ranges {
		if c.Start == r.Start && c.End == r.End {
			return true
		}
	}

	return false
}

// CaughtUp reports whether this shard has finished being copied into for a
// range: the backfill walked it to the end, and the log has kept it current
// since.
//
// This is what a promotion is decided on, and it is deliberately the only way to
// ask. A learner holding *some* of a range is the one state that must not be
// served — a promoted learner answers "no such object" for every key the copy
// has not reached, and nothing reports it, because a partial range is a range
// that simply says no.
//
// # In memory, and only while the range is still being learned
//
// Not persisted, and the omission is the safety. A durable claim of doneness
// outlives the data behind it: a node promoted, later moved away and later still
// made a learner of that range again would read its own old claim and report
// ready without copying anything. So a restart forgets, and forgetting costs one
// re-copy — against a promotion onto data this node does not have.
//
// Configure prunes it to the ranges still being learned, which gives the same
// protection without a restart: a completion survives exactly as long as the
// instruction that produced it.
func (s *Shard) CaughtUp(r rangemap.Range) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.caught[idOf(r)]
}

// markCaughtUp records that a range has been copied in full.
func (s *Shard) markCaughtUp(r rangemap.Range) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.caught == nil {
		s.caught = make(map[rangeID]bool, 1)
	}

	s.caught[idOf(r)] = true
}

// retainCaughtUp drops every completion that is not for one of these ranges. The
// caller holds the lock.
func (s *Shard) retainCaughtUp(learning []rangemap.Range) {
	if len(s.caught) == 0 {
		return
	}

	keep := make(map[rangeID]bool, len(learning))

	for _, r := range learning {
		if id := idOf(r); s.caught[id] {
			keep[id] = true
		}
	}

	s.caught = keep
}

// rangeID identifies a range for the purpose of remembering it was copied.
//
// The owner is part of it because a copy is a copy of a particular node's
// contents: after a failover the range has the same bounds and different
// contents — the promoted node holds the records it received and not the ones it
// did not — and a completion remembered against the old owner would skip the
// difference.
type rangeID struct {
	start string
	end   string
	owner cluster.NodeID
}

func idOf(r rangemap.Range) rangeID {
	return rangeID{start: r.Start, end: r.End, owner: r.Owner}
}

// ApplyBatch replays an owner's batch into this shard.
//
// The batch is applied as the owner recorded it — entry and counter together,
// in one atomic write — so a follower's state is the owner's state, not a
// reconstruction of it. That is what makes a promotion cheap: there is nothing
// to reconcile, only a range to start serving.
func (s *Shard) ApplyBatch(ctx context.Context, r rangemap.Range, repr []byte) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "apply batch")
	}

	if !s.replicates(r) {
		return ErrNotFollowed
	}

	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()

	if err := batch.SetRepr(repr); err != nil {
		return errors.Wrap(err, "decode batch")
	}

	// Synced, unlike the owner's own writes. The owner can afford unsynced
	// writes because a crash costs it a rebuild from the disks; a follower
	// exists precisely to be current when the owner is gone, and one that lost
	// its tail to a crash would be promoted into exactly the gap it was there
	// to prevent.
	if err := batch.Commit(pebble.Sync); err != nil {
		return errors.Wrap(err, "apply batch")
	}

	return nil
}

// shipTo hands a committed batch to the followers of the range it belongs to.
//
// Called with the batch's own representation after the commit succeeded. A
// range with no followers ships nothing, which is the R=1 configuration: legal,
// and it means a lost owner costs a rebuild rather than a promotion.
func (s *Shard) shipTo(ctx context.Context, key, repr []byte) {
	s.mu.RLock()
	ship := s.ship
	s.mu.RUnlock()

	if ship == nil {
		return
	}

	r, ok := s.rangeFor(key)
	// Learners count. A learner is being backfilled and the log is what keeps
	// it current for everything written meanwhile — a backfill that copied a
	// moving target and received none of the changes would finish holding the
	// range as it was when the copy started.
	if !ok || (len(r.Followers) == 0 && len(r.Learners) == 0) {
		return
	}

	ship(ctx, r, repr)
}

// rangeFor returns the served range holding an encoded key.
func (s *Shard) rangeFor(key []byte) (rangemap.Range, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, r := range s.owned {
		if r.Contains(string(key)) {
			return r, true
		}
	}

	return rangemap.Range{}, false
}
