package main

import (
	"context"
	"sync"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// rebalanceController is the server-side rebalance runner behind the admin
// API: the same elected, cursor-checkpointed walk as `fs cluster rebalance`,
// driven by this node's repairer. Starting it on several nodes leaves the
// extras campaigning as standbys; pausing keeps the etcd cursor so any node
// resumes the walk.
type rebalanceController struct {
	lg       *zap.Logger
	client   *clientv3.Client
	etcdCfg  etcd.Config
	repairer *clusterstore.Repairer
	coord    *clusterstore.Coordinator
	// candidate labels this node in the election.
	candidate string
	// baseCtx bounds every run: the server's lifetime, not the API request's.
	baseCtx context.Context

	mu     sync.Mutex
	state  adminhandler.RebalanceState
	cancel context.CancelFunc
	// gen identifies the current run: a runner goroutine from a superseded run
	// (paused, then relaunched before it exited) must not touch the state.
	gen int
	// pauseRequested distinguishes an operator pause from a run failure when
	// the runner goroutine exits on a canceled context.
	pauseRequested bool

	objects, relocated, failed int
	startedAt, finishedAt      time.Time
	lastErr                    string
}

// newRebalanceController builds the controller; ctx bounds all runs.
func newRebalanceController(ctx context.Context, lg *zap.Logger, client *clientv3.Client, etcdCfg etcd.Config, coord *clusterstore.Coordinator, repairer *clusterstore.Repairer, candidate string) *rebalanceController {
	return &rebalanceController{
		lg:        lg,
		client:    client,
		etcdCfg:   etcdCfg,
		repairer:  repairer,
		coord:     coord,
		candidate: candidate,
		baseCtx:   ctx,
		state:     adminhandler.RebalanceIdle,
	}
}

var _ adminhandler.RebalanceControl = (*rebalanceController)(nil)

// Start implements adminhandler.RebalanceControl.
func (c *rebalanceController) Start(context.Context) error {
	return c.launch(false)
}

// Resume implements adminhandler.RebalanceControl. The cursor lives in etcd,
// so resuming is a relaunch that requires a paused runner.
func (c *rebalanceController) Resume(context.Context) error {
	return c.launch(true)
}

// launch transitions to waiting and spawns the runner goroutine.
func (c *rebalanceController) launch(resume bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case adminhandler.RebalanceWaiting, adminhandler.RebalanceRunning:
		return errors.Wrap(adminhandler.ErrRebalanceConflict, "rebalance already in progress on this node")
	case adminhandler.RebalancePaused, adminhandler.RebalanceIdle, adminhandler.RebalanceDone, adminhandler.RebalanceFailed:
	}

	if resume && c.state != adminhandler.RebalancePaused {
		return errors.Wrap(adminhandler.ErrRebalanceConflict, "no paused rebalance to resume")
	}

	runCtx, cancel := context.WithCancel(c.baseCtx)

	c.state = adminhandler.RebalanceWaiting
	c.cancel = cancel
	c.gen++
	c.pauseRequested = false
	c.objects, c.relocated, c.failed = 0, 0, 0
	c.startedAt = time.Now()
	c.finishedAt = time.Time{}
	c.lastErr = ""

	go c.run(runCtx, cancel, c.gen)

	return nil
}

// Pause implements adminhandler.RebalanceControl.
func (c *rebalanceController) Pause(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case adminhandler.RebalanceWaiting, adminhandler.RebalanceRunning:
	default:
		return errors.Wrap(adminhandler.ErrRebalanceConflict, "no rebalance in progress on this node")
	}

	// The runner goroutine observes the flag on exit and lands on "paused";
	// flip the state now so the API reflects the pause immediately.
	c.pauseRequested = true
	c.state = adminhandler.RebalancePaused
	c.cancel()

	return nil
}

// Status implements adminhandler.RebalanceControl.
func (c *rebalanceController) Status(ctx context.Context) adminhandler.RebalanceStatus {
	c.mu.Lock()
	st := adminhandler.RebalanceStatus{
		State:      c.state,
		Objects:    c.objects,
		Relocated:  c.relocated,
		Failed:     c.failed,
		StartedAt:  c.startedAt,
		FinishedAt: c.finishedAt,
		Err:        c.lastErr,
	}
	c.mu.Unlock()

	st.RepairQueueDepth = c.coord.QueueDepth()

	// Best-effort cursor read: status must not fail with etcd.
	if raw, ok, err := etcd.LoadRebalanceCursor(ctx, c.client, c.etcdCfg); err == nil && ok {
		if cur, err := clusterstore.DecodeRebalanceCursor(raw); err == nil {
			st.CursorBucket, st.CursorKey = cur.Bucket, cur.Key
		}
	}

	return st
}

// run campaigns for the cluster-wide slot and drives the rebalance walk,
// then settles the terminal state.
func (c *rebalanceController) run(ctx context.Context, cancel context.CancelFunc, gen int) {
	defer cancel()

	err := c.runElected(ctx, cancel, gen)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.gen != gen {
		return // A newer run owns the state (paused, then relaunched).
	}

	c.finishedAt = time.Now()

	if c.pauseRequested {
		return // Pause already set the state.
	}

	if err != nil {
		c.state = adminhandler.RebalanceFailed
		c.lastErr = err.Error()

		c.lg.Warn("Cluster rebalance failed", zap.Error(err))

		return
	}

	c.state = adminhandler.RebalanceDone

	c.lg.Info("Cluster rebalance done",
		zap.Int("objects", c.objects),
		zap.Int("relocated", c.relocated),
		zap.Int("failed", c.failed),
	)
}

// runElected is the elected walk: campaign, resume from the etcd cursor,
// checkpoint per batch, clear the cursor on completion.
func (c *rebalanceController) runElected(ctx context.Context, cancel context.CancelFunc, gen int) error {
	c.lg.Info("Campaigning for cluster rebalance leadership", zap.String("candidate", c.candidate))

	lead, err := etcd.CampaignRebalance(ctx, c.client, c.etcdCfg, c.candidate)
	if err != nil {
		return errors.Wrap(err, "campaign")
	}

	defer func() { _ = lead.Close() }()

	// Lost leadership (lease expiry) must stop the walk: a standby resumes
	// from the last checkpoint.
	go func() {
		select {
		case <-lead.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	var resume clusterstore.RebalanceCursor

	if raw, ok, err := etcd.LoadRebalanceCursor(ctx, c.client, c.etcdCfg); err != nil {
		return err
	} else if ok {
		if resume, err = clusterstore.DecodeRebalanceCursor(raw); err != nil {
			return err
		}
	}

	c.mu.Lock()
	if c.gen == gen && !c.pauseRequested {
		c.state = adminhandler.RebalanceRunning
	}
	c.mu.Unlock()

	c.lg.Info("Cluster rebalance elected and running", zap.String("resume_bucket", resume.Bucket), zap.String("resume_key", resume.Key))

	_, err = c.repairer.Rebalance(ctx, clusterstore.RebalanceOptions{
		Resume: resume,
		OnObject: func(_, _ string, rep *clusterstore.RepairReport, err error) {
			c.mu.Lock()
			defer c.mu.Unlock()

			if c.gen != gen {
				return
			}

			c.objects++

			switch {
			case err != nil:
				c.failed++
			case rep.Changed():
				c.relocated++
			}
		},
		Checkpoint: func(ctx context.Context, cur clusterstore.RebalanceCursor) error {
			raw, err := cur.Encode()
			if err != nil {
				return err
			}

			return lead.SaveCursor(ctx, raw)
		},
	})
	if err != nil {
		return err
	}

	if err := lead.ClearCursor(context.WithoutCancel(ctx)); err != nil {
		c.lg.Warn("Clearing rebalance cursor failed", zap.Error(err))
	}

	return nil
}
