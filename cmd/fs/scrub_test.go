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

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	// An hour between passes, nothing recorded: the first has to land now.
	go scrubLoop(ctx, lg, storage, IntegrityConfig{ScrubInterval: time.Hour},
		lastrun.NewFile(t.TempDir()), time.Millisecond)

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

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	go scrubLoop(runCtx, lg, storage, IntegrityConfig{ScrubInterval: time.Hour}, state, time.Millisecond)

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

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	go scrubLoop(runCtx, lg, storage, IntegrityConfig{ScrubInterval: time.Hour}, state, time.Millisecond)

	require.Eventually(t, func() bool {
		return scrubbed(logs) > 0
	}, 5*time.Second, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		at, err := state.LastRun(ctx, scrubTask)
		require.NoError(t, err)

		return !at.IsZero()
	}, 5*time.Second, 5*time.Millisecond, "a completed scrub must be recorded")
}

// TestClusterScrubTaskIsPerNode: each node scrubs its own disks, so one node
// completing a pass says nothing about another's objects. A shared key would
// let a cluster hold every node's scrub off on the strength of whichever ran
// last.
func TestClusterScrubTaskIsPerNode(t *testing.T) {
	t.Parallel()

	require.NotEqual(t, clusterScrubTask("node-a"), clusterScrubTask("node-b"))
	require.NotEqual(t, scrubTask, clusterScrubTask("node-a"))
}
