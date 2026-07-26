package clusterstore

import (
	"context"
	"time"

	"github.com/go-faster/fs/internal/cluster"
)

// ScrubStateStore persists per-disk scrub progress so an interrupted pass
// resumes instead of restarting.
//
// It is deliberately node-local. A scrub cursor describes disks only this node
// can read, so no other node could resume from it, and the control plane is the
// wrong home for state nobody else can use: it would mean a replicated, fsynced
// write every few hundred objects from every disk of every node, against an
// etcd budgeted for kilobytes of control-plane state — and it would stop a
// local durability process from making progress during a control-plane outage.
//
// The interface is declared here, where it is consumed, and satisfied
// structurally by diskstore, which owns the disk roots. Neither package imports
// the other.
//
// A nil store disables resuming: every pass then starts from the beginning,
// which is the behavior this replaced.
type ScrubStateStore interface {
	// LoadScrubState returns the disk's recorded progress. An unknown or
	// unreadable disk returns the zero state, not an error: losing a cursor
	// costs a restarted pass, never correctness.
	LoadScrubState(disk cluster.DiskID) cluster.ScrubState
	// SaveScrubState records progress. Errors are advisory — the caller keeps
	// scrubbing, because failing to save a cursor is not a reason to stop
	// verifying data.
	SaveScrubState(disk cluster.DiskID, state cluster.ScrubState) error
}

// FragmentWalker streams the fragment names on one of this node's disks, in
// lexicographic order, without materializing them.
//
// The scrubber used to ask the peer transport for the whole listing, which
// meant every name on the disk — several per object — held as a string before
// the first one could be looked at. On a disk holding tens of millions of
// fragments that is gigabytes of strings, and it was the first thing to fail on
// a large node.
//
// Order is the only thing the sweep needs: an object's entries are contiguous
// in it, so each namespace can be handled and dropped before the next begins.
//
// after is a hint. An implementation may prune names at or before it — that is
// what makes resuming a pass cheap — so the caller applies its own boundary and
// must not depend on receiving them.
//
// A nil walker falls back to a buffered listing over the transport, which is
// correct and is what every non-cluster deployment and most tests use.
type FragmentWalker interface {
	WalkFragments(ctx context.Context, disk cluster.DiskID, after string, fn func(name string) error) error
}

// VerificationIndex records when the scrub last checked an object, and answers
// when it last did.
//
// It replaces the set of objects a pass has already swept, which was held in
// memory and grew with the objects on the node — the last thing in the scrub
// that did. Two of a node's disks can hold the same object under different
// epochs, and repairing it twice per pass is waste; asking when it was last
// verified answers that without remembering every key.
//
// Implementations may batch what they record: a scrub verifying millions of
// objects should not pay a durable write for each, and the cost of losing the
// last few stamps to a crash is re-verifying those objects. LastVerified must
// see what has been recorded but not yet flushed, or the same object is swept
// twice within one pass — the very thing this replaces.
//
// A nil index keeps the in-memory set, which is what a node without one does.
type VerificationIndex interface {
	// LastVerified reports when an object was last checked; false when never,
	// or when the index does not hold it.
	LastVerified(bucket, key string) (time.Time, bool)
	// RecordVerified notes that an object has just been checked.
	RecordVerified(bucket, key string, at time.Time)
	// Flush makes recorded verifications durable.
	Flush() error
}
