package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/go-faster/fs/internal/lastrun"
	"github.com/go-faster/fs/storagefs"
)

// runScrubLoop runs the scrub loop with an hour between passes until the test
// ends, and waits for it to stop before the test's directories are removed: a
// pass still writing its record would otherwise race the cleanup, which
// Windows refuses outright.
func runScrubLoop(t *testing.T, lg *zap.Logger, storage *storagefs.Storage, state lastrun.Store) {
	t.Helper()

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		scrubLoop(ctx, lg, storage, IntegrityConfig{ScrubInterval: time.Hour}, state, time.Millisecond)
	}()

	t.Cleanup(func() {
		stop()
		<-done
	})
}

// scrubFixture returns an empty store and a log to watch passes on.
func scrubFixture(t *testing.T) (*storagefs.Storage, *zap.Logger, *observer.ObservedLogs) {
	t.Helper()

	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	core, logs := observer.New(zap.InfoLevel)

	return storage, zap.New(core), logs
}

func scrubbed(logs *observer.ObservedLogs) int {
	return len(logs.FilterMessage("Scrub complete").All())
}

// TestScrubLoopScrubsWhenOverdue is the guard on restart behavior.
//
// The loop must not wait a whole interval for a scrub that is already due: a
// node restarted more often than scrub_interval would then never scrub at all,
// and a deployment that redeploys daily with a 24h interval would go unscrubbed
// forever while the log claimed the scrubber was running.
func TestScrubLoopScrubsWhenOverdue(t *testing.T) {
	t.Parallel()

	storage, lg, logs := scrubFixture(t)

	// An hour between passes, nothing recorded: the first has to land now.
	runScrubLoop(t, lg, storage, lastrun.NewFile(t.TempDir()))

	require.Eventually(t, func() bool {
		return scrubbed(logs) > 0
	}, 5*time.Second, 5*time.Millisecond, "an overdue scrub must not wait a full interval")
}

// TestScrubLoopHonorsARecentScrub is the other half, and the reason the record
// exists: a pass that just ran must not be repeated because the process
// restarted. Without it a node restarting every few minutes re-walks every
// object each time — the load the schedule is supposed to bound.
func TestScrubLoopHonorsARecentScrub(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage, lg, logs := scrubFixture(t)

	state := lastrun.NewFile(t.TempDir())
	require.NoError(t, state.SetLastRun(ctx, scrubTask, time.Now()))

	runScrubLoop(t, lg, storage, state)

	require.Never(t, func() bool {
		return scrubbed(logs) > 0
	}, 250*time.Millisecond, 25*time.Millisecond, "a scrub recorded moments ago must not run again on start")
}

// TestScrubLoopRecordsItsPass: the pass has to leave the record behind, or the
// next start has nothing to schedule from and scrubs again.
func TestScrubLoopRecordsItsPass(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage, lg, logs := scrubFixture(t)
	state := lastrun.NewFile(t.TempDir())

	runScrubLoop(t, lg, storage, state)

	require.Eventually(t, func() bool {
		return scrubbed(logs) > 0
	}, 5*time.Second, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		at, err := state.LastRun(ctx, scrubTask)
		require.NoError(t, err)

		return !at.IsZero()
	}, 5*time.Second, 5*time.Millisecond, "a completed scrub must be recorded")
}
