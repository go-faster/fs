package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/metastore"
)

// planeFixture is a controller over a walk a test drives by hand.
//
// The walk runs on its own goroutine — that is the property under test — so
// everything it touches is guarded. An unguarded counter here passed locally
// and failed under -race in CI, which is the whole argument for the mutex being
// in a test fixture at all.
type planeFixture struct {
	*planeController

	// release lets a test hold the walk open, so "already running" is a state
	// it can observe rather than a race it has to win.
	release chan struct{}
	done    chan struct{}

	mu     sync.Mutex
	runs   int
	ranCtx context.Context
	err    error
	build  metastore.Build
}

// ran records that the walk started, and on which context.
func (f *planeFixture) ran(ctx context.Context) error {
	f.mu.Lock()
	f.runs++
	f.ranCtx = ctx
	err := f.err
	f.mu.Unlock()

	<-f.release

	f.done <- struct{}{}

	return err
}

// runCount is how many walks have started.
func (f *planeFixture) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.runs
}

// ranContext is the context the walk was given, or nil before it started.
func (f *planeFixture) ranContext() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.ranCtx
}

func newPlaneFixture(t *testing.T, build metastore.Build) *planeFixture {
	t.Helper()

	f := &planeFixture{
		release: make(chan struct{}),
		done:    make(chan struct{}, 8),
		build:   build,
	}

	f.planeController = &planeController{
		lg:      zaptest.NewLogger(t),
		policy:  RebuildOnFailure,
		baseCtx: t.Context(),
		status: func(context.Context) (metastore.Build, error) {
			return f.build, nil
		},
		run: f.ran,
	}

	return f
}

// finish releases the walk and waits for the runner to record the outcome.
func (f *planeFixture) finish(t *testing.T) {
	t.Helper()

	close(f.release)

	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the rebuild never finished")
	}

	require.Eventually(t, func() bool { return !f.Running() }, 5*time.Second, time.Millisecond)
}

// TestAnOperatorCanStartARebuildNow is what #191 asks for: "now" expressible
// without a shell on a node — and it must ignore the policy, since the case it
// exists for is the one the policy deliberately leaves alone.
func TestAnOperatorCanStartARebuildNow(t *testing.T) {
	f := newPlaneFixture(t, metastore.Building(metastore.CauseNeverBuilt))

	require.False(t, rebuildWanted(f.policy, metastore.CauseNeverBuilt),
		"the premise: nothing would start this on its own")

	require.NoError(t, f.Rebuild(t.Context()))

	got, err := f.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, got.Rebuilding)
	assert.False(t, got.Ready)
	assert.Equal(t, "never-built", got.Cause)

	f.finish(t)
	assert.Equal(t, 1, f.runCount())
}

// TestRebuildOutlivesTheRequest: a walk is hours, and a request is not. Run on
// the request's context it would be canceled the moment the response was
// written, and an operator would see a rebuild start and silently stop.
func TestRebuildOutlivesTheRequest(t *testing.T) {
	f := newPlaneFixture(t, metastore.Building(metastore.CauseNeverBuilt))

	reqCtx, cancel := context.WithCancel(t.Context())

	require.NoError(t, f.Rebuild(reqCtx))

	// The request is over.
	cancel()

	require.Eventually(t, func() bool { return f.ranContext() != nil }, 5*time.Second, time.Millisecond)
	assert.NoError(t, f.ranContext().Err(),
		"the rebuild was canceled with the request that asked for it")

	f.finish(t)
}

// TestOneRebuildPerNode: the timer and an operator both start rebuilds, and
// both campaign. Two at once on one node would leave the second blocked on the
// election for a whole rebuild, doing nothing.
func TestOneRebuildPerNode(t *testing.T) {
	f := newPlaneFixture(t, metastore.Building(metastore.CauseOrphaned))

	require.NoError(t, f.Rebuild(t.Context()))

	err := f.Rebuild(t.Context())
	require.ErrorIs(t, err, adminhandler.ErrPlaneRebuildConflict)

	// And the timed policy goes through the same guard.
	require.ErrorIs(t, f.start(nil), adminhandler.ErrPlaneRebuildConflict)

	f.finish(t)
	assert.Equal(t, 1, f.runCount(), "two rebuilds ran on one node")
}

// TestARebuildCanBeStartedAgainOnceItIsDone: the guard is "running", not "ever
// ran". A rebuild that failed must be retryable, and one that succeeded must be
// repeatable — a plane can be orphaned twice.
func TestARebuildCanBeStartedAgainOnceItIsDone(t *testing.T) {
	f := newPlaneFixture(t, metastore.Building(metastore.CauseOrphaned))

	require.NoError(t, f.Rebuild(t.Context()))
	f.finish(t)

	f.release = make(chan struct{})
	require.NoError(t, f.Rebuild(t.Context()))
	f.finish(t)

	assert.Equal(t, 2, f.runCount())
}

// TestAFailedRebuildIsReported: it ran hours ago and the request that asked for
// it returned long before, so the only place the failure can be seen is here.
func TestAFailedRebuildIsReported(t *testing.T) {
	f := newPlaneFixture(t, metastore.Building(metastore.CauseOrphaned))
	f.mu.Lock()
	f.err = errors.New("etcd went away")
	f.mu.Unlock()

	require.NoError(t, f.Rebuild(t.Context()))
	f.finish(t)

	got, err := f.Status(t.Context())
	require.NoError(t, err)

	assert.False(t, got.Rebuilding)
	assert.Contains(t, got.Err, "etcd went away")
	assert.False(t, got.FinishedAt.IsZero())
}

// TestAReadyPlaneReportsNoCause: the cause describes why a plane is unusable,
// and a usable one has no such reason. Reporting the last one would have an
// operator reading a stale explanation of a state the cluster has left.
func TestAReadyPlaneReportsNoCause(t *testing.T) {
	f := newPlaneFixture(t, metastore.Ready())

	got, err := f.Status(t.Context())
	require.NoError(t, err)

	assert.True(t, got.Ready)
	assert.Empty(t, got.Cause)
}

// TestStatusReportsThePolicy: an operator looking at a plane that has been
// building for an hour needs to know whether anything is going to act on it.
func TestStatusReportsThePolicy(t *testing.T) {
	f := newPlaneFixture(t, metastore.Building(metastore.CauseNeverBuilt))

	got, err := f.Status(t.Context())
	require.NoError(t, err)

	assert.Equal(t, RebuildOnFailure, got.Policy)
}
