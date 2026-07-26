package clusterstore

import "github.com/go-faster/fs/internal/cluster"

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
