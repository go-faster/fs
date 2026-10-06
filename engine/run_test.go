package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-faster/errors"
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

// TestSweepLoopRetriesAFailedSweep pins that a sweep that fails — the first one
// of a new layout often does, while gossip catches up — runs again soon rather
// than a full period later, and then backs off.
func TestSweepLoopRetriesAFailedSweep(t *testing.T) {
	defer func(d time.Duration) { syncRetry = d }(syncRetry)

	syncRetry = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		mu    sync.Mutex
		calls int
	)

	done := make(chan struct{})
	sweep := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()

		calls++

		switch calls {
		case 1, 2:
			return errors.New("no address known for node")
		case 3:
			close(done)
		}

		return nil
	}

	go sweepLoop(ctx, time.Hour, func() uint64 { return 1 }, sweep, nil)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a failed sweep was not retried before the hour-long period")
	}

	// Once one succeeds, nothing runs again until the layout moves or the
	// period passes.
	time.Sleep(2500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if calls != 3 {
		t.Errorf("%d sweeps, want 3: the loop kept sweeping after a success", calls)
	}
}
