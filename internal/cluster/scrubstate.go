package cluster

import "time"

// ScrubState is one disk's scrub progress, durable across restarts.
//
// A deep scrub reads every payload on the disk, so a pass over a large disk
// runs for hours or days. Without somewhere to record how far it got, a restart
// — a rolling upgrade, an OOM, a crash — throws the whole pass away and the
// next one starts at the beginning. A node restarted more often than a sweep
// takes then verifies the front of each disk over and over and never reaches
// the back, while every counter says the scrubber is working.
//
// It lives here, with the other domain types, so the store that persists it and
// the repairer that keeps it need not know about each other.
type ScrubState struct {
	// Cursor is the last object namespace fully processed, in the walk's
	// lexicographic order. Empty means no pass is in flight: the next one
	// starts from the beginning.
	Cursor string
	// PassStarted is when the in-flight pass began.
	PassStarted time.Time
	// LastCompleted is when a pass over this disk last ran to the end. It is
	// the honest measure of scrub coverage — how long ago every object on this
	// disk was last verified — and the one number that reveals a cycle failing
	// to keep up, which counters of work done cannot.
	LastCompleted time.Time
}

// InProgress reports whether a pass was interrupted and has somewhere to resume
// from.
func (s ScrubState) InProgress() bool { return s.Cursor != "" }
