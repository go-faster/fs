package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGCLoopSurvivesRestarts checks the first collection is timed from the
// last one recorded, not from start: a node restarted more often than the
// period still collects, and one that just did waits.
func TestGCLoopSurvivesRestarts(t *testing.T) {
	gcFloor = 10 * time.Millisecond

	t.Cleanup(func() { gcFloor = time.Minute })

	e := newEngine(t, 1)

	run := func(last time.Time) int64 {
		e.setLastGC(last)

		var passes atomic.Int64

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		e.gcLoop(ctx, time.Hour, func() error {
			passes.Add(1)

			return nil
		})

		return passes.Load()
	}

	assert.Zero(t, run(time.Now()), "a pass just recorded is not due for an hour")

	before := time.Now()

	require.Equal(t, int64(1), run(time.Now().Add(-2*time.Hour)), "an overdue pass runs after the floor")
	assert.False(t, e.lastGC().Before(before), "and is recorded")
}
