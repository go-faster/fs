// Package lastrun records when a periodic background pass last completed, so
// the pass survives restarts without either re-running on every one of them or
// being postponed forever by them.
//
// Without it a periodic loop has to choose between two wrong answers. Arm a
// ticker and the first pass is a whole interval away, so a node restarted more
// often than the interval never runs it at all. Run on start instead and a node
// that restarts often runs it constantly — a full object walk every deploy.
// Neither is what an operator who wrote "every 24 hours" asked for.
//
// Remembering when the pass last finished answers both: the next one is due
// interval after that, whoever is running now and however many times the
// process has restarted since.
package lastrun

import (
	"context"
	"time"
)

// Store persists the completion time of a named pass.
//
// task names the pass ("scrub", "lifecycle"), and is the whole key: a store
// holds one timestamp per task. Callers that need a per-node record put the
// node in the task name, because whether a pass is per-node or cluster-wide is
// a property of the pass and not of the storage.
type Store interface {
	// LastRun returns when task last completed. A zero time means it never has
	// — a fresh deployment, or one that predates this record — which callers
	// treat as "due now".
	LastRun(ctx context.Context, task string) (time.Time, error)
	// SetLastRun records that task completed at t.
	SetLastRun(ctx context.Context, task string, t time.Time) error
}

// Due returns how long to wait before the next pass of a loop that runs every
// interval, given when it last completed.
//
// floor is the shortest wait it will ever return, and is what stops a
// crashlooping node from re-running an overdue pass on every restart: the pass
// is recorded only when it finishes, so a node that dies mid-pass would
// otherwise start another one immediately on each restart, forever.
//
// A pass that is not yet due waits out its remaining time. One that is overdue
// — including one that has never run — waits only the floor.
func Due(last, now time.Time, interval, floor time.Duration) time.Duration {
	if last.IsZero() {
		return floor
	}

	remaining := interval - now.Sub(last)
	if remaining < floor {
		return floor
	}

	// A clock that jumped backwards, or a record written by a node whose clock
	// runs ahead, would otherwise park the pass for longer than the interval
	// the operator asked for.
	if remaining > interval {
		return interval
	}

	return remaining
}
