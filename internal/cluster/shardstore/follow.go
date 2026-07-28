package shardstore

import (
	"context"

	"github.com/cockroachdb/pebble/v2"
	"github.com/go-faster/errors"

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

// Configure sets what this shard serves and what it replicates, together.
//
// Together rather than as an Adopt followed by a Follow, because between those
// two the shard would be running on half of one map and half of another. The
// half that matters is promotion: a node that has just taken over a range would
// briefly both own it and follow it, and the deposed owner is exactly the node
// most likely to still be shipping batches for it.
func (s *Shard) Configure(owned, followed []rangemap.Range) {
	o := make([]rangemap.Range, len(owned))
	copy(o, owned)

	f := make([]rangemap.Range, len(followed))
	copy(f, followed)

	s.mu.Lock()
	s.owned, s.followed = o, f
	s.mu.Unlock()
}

// Following returns the ranges this shard replicates, in key order.
func (s *Shard) Following() []rangemap.Range {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]rangemap.Range, len(s.followed))
	copy(out, s.followed)

	return out
}

// replicates reports whether this shard may replay a batch for a range: it is
// in the followed set, and it is not one this shard owns.
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

	for _, o := range s.owned {
		if o.Start == r.Start && o.End == r.End {
			return false
		}
	}

	for _, f := range s.followed {
		if f.Start == r.Start && f.End == r.End {
			return true
		}
	}

	return false
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
