package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/go-faster/fs/storagefs"
)

func TestFirstPass(t *testing.T) {
	t.Parallel()

	// A long cadence gets the short delay: the point is that a restart does not
	// postpone the pass by a whole interval.
	require.Equal(t, firstPassDelay, firstPass(24*time.Hour))
	require.Equal(t, firstPassDelay, firstPass(firstPassDelay))

	// A cadence shorter than the delay is its own answer; waiting longer than
	// the operator asked for would be its own surprise.
	require.Equal(t, time.Minute, firstPass(time.Minute))
	require.Equal(t, time.Duration(0), firstPass(0))
}

// TestRunScrubberScrubsBeforeTheFirstInterval is the guard on restart behavior.
//
// The loop must not wait a whole interval for its first pass: a node restarted
// more often than scrub_interval would then never scrub at all, and a
// deployment that redeploys daily with a 24h interval would go unscrubbed
// forever while the log claimed the scrubber was running.
func TestRunScrubberScrubsBeforeTheFirstInterval(t *testing.T) {
	t.Parallel()

	storage, err := storagefs.New(t.TempDir())
	require.NoError(t, err)

	core, logs := observer.New(zap.InfoLevel)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	// An hour between passes; the first one has to land long before that.
	go scrubLoop(ctx, zap.New(core), storage, IntegrityConfig{ScrubInterval: time.Hour}, time.Millisecond)

	require.Eventually(t, func() bool {
		return len(logs.FilterMessage("Scrub complete").All()) > 0
	}, 5*time.Second, 5*time.Millisecond, "the first scrub must not wait a full interval")
}
