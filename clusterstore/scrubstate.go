package clusterstore

import (
	"context"

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
