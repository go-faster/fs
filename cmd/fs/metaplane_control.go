package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/metastore"
)

// planeController is the sharded metadata plane behind the admin API, and the
// one guard both ways of starting a rebuild go through.
//
// Two things start rebuilds: the timed policy, which acts when a failure left a
// range with no copy of its data, and an operator, who acts when they have
// decided the moment. They must not run at once on one node. Both would
// campaign, so the second would block on the election until the first finished
// — safe, and a whole rebuild's worth of a goroutine waiting to do nothing.
type planeController struct {
	lg *zap.Logger
	// run is the elected, cursor-checkpointed walk. Injected rather than
	// called through the runtime so a test can drive this without a cluster.
	run func(context.Context) error
	// status reads the plane's flag and why it is set.
	status func(context.Context) (metastore.Build, error)
	// policy is the configured automatic-rebuild policy, reported so an
	// operator can see why nothing has happened on its own.
	policy string
	// baseCtx bounds every run: the server's lifetime, not the API request's.
	//
	// A rebuild outlives the request that asked for it by hours. Run on the
	// request's context it would be canceled the moment the response was
	// written, and an operator would see a rebuild start and silently stop.
	baseCtx context.Context

	mu                    sync.Mutex
	running               bool
	startedAt, finishedAt time.Time
	lastErr               string
}

var _ adminhandler.PlaneControl = (*planeController)(nil)

// Status implements adminhandler.PlaneControl.
func (c *planeController) Status(ctx context.Context) (adminhandler.PlaneStatus, error) {
	build, err := c.status(ctx)
	if err != nil {
		return adminhandler.PlaneStatus{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	out := adminhandler.PlaneStatus{
		Ready:      build.State == metastore.StateReady,
		Rebuilding: c.running,
		Policy:     c.policy,
		StartedAt:  c.startedAt,
		FinishedAt: c.finishedAt,
		Err:        c.lastErr,
	}

	if !out.Ready {
		out.Cause = build.Cause.String()
	}

	return out, nil
}

// Rebuild implements adminhandler.PlaneControl: start now, whatever the policy.
//
// Returns as soon as the walk is launched. It is hours on a cluster of any
// size, so a request that waited for it would time out long before it finished
// and leave an operator unable to tell a rebuild that was running from one that
// never started.
func (c *planeController) Rebuild(context.Context) error {
	return c.start(func() {
		c.lg.Info("Metadata plane rebuild requested by an operator",
			zap.String("policy", c.policy))
	})
}

// start launches a rebuild unless this node is already running one.
//
// The whole point of the type: the timer and the operator both come through
// here, so "already running" is one fact rather than two that have to agree.
func (c *planeController) start(announce func()) error {
	c.mu.Lock()

	if c.running {
		c.mu.Unlock()

		return adminhandler.ErrPlaneRebuildConflict
	}

	c.running = true
	c.startedAt = time.Now()
	c.finishedAt = time.Time{}
	c.lastErr = ""

	c.mu.Unlock()

	if announce != nil {
		announce()
	}

	go c.walk()

	return nil
}

// walk runs the rebuild and records what happened to it.
func (c *planeController) walk() {
	err := c.run(c.baseCtx)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.running = false
	c.finishedAt = time.Now()

	if err != nil {
		c.lastErr = err.Error()

		// Logged here rather than left to the caller: the caller is an HTTP
		// request that returned hours ago, or a ticker that has moved on.
		c.lg.Warn("Metadata plane rebuild failed", zap.Error(err))

		return
	}

	c.lg.Info("Metadata plane rebuild finished")
}

// Running reports whether this node is rebuilding, for the timed policy to
// avoid starting a second one.
func (c *planeController) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.running
}
