package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/lifecycle"
)

// runLifecycle enforces bucket lifecycle rules on a single node until ctx is
// canceled.
//
// A disabled sweep is reported at warning level rather than passed over in
// silence. The ?lifecycle subresource still accepts rules — it is the same
// bucket metadata either way — so an operator who turns enforcement off has a
// server that stores expiry rules and deletes nothing, and that has to be
// visible in the log rather than discovered from objects that never went away.
func runLifecycle(ctx context.Context, lg *zap.Logger, storage fs.Storage, cfg LifecycleConfig) {
	if cfg.Interval <= 0 {
		lg.Warn("Lifecycle enforcement is disabled; rules clients set will be stored but never applied")
		return
	}

	sweeper := &lifecycle.Sweeper{Storage: storage, Log: lg}
	sweeper.Run(ctx, cfg.Interval)
}

// RunLifecycle enforces bucket lifecycle rules from exactly one node.
//
// It campaigns first, for the same reason the usage recount does: a pass lists
// every bucket that has rules and deletes through the ordinary path, so running
// it on each node would multiply the listing by the node count and have every
// node race the others to delete the same keys. A node that loses stands by and
// takes over when the holder's lease expires.
func (rt *clusterRuntime) RunLifecycle(ctx context.Context, cfg LifecycleConfig) {
	if cfg.Interval <= 0 {
		rt.lg.Warn("Lifecycle enforcement is disabled; rules clients set will be stored but never applied")
		return
	}

	sweeper := &lifecycle.Sweeper{Storage: rt.Storage, Log: rt.lg}

	for ctx.Err() == nil {
		lead, err := etcd.CampaignLifecycle(ctx, rt.client, rt.etcdCfg, string(rt.nodeID))
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			rt.lg.Warn("Lifecycle election failed", zap.Error(err))

			if !sleepCtx(ctx, cfg.Interval) {
				return
			}

			continue
		}

		rt.lg.Debug("Holding the lifecycle sweep leadership")

		// Losing the lease mid-pass must stop the sweep, not merely end it
		// after the current one: the node that took over is already deleting,
		// and two sweepers racing turn every expiry into a contested delete.
		held, stop := context.WithCancel(ctx)

		go func() {
			select {
			case <-held.Done():
			case <-lead.Done():
				stop()
			}
		}()

		sweeper.Run(held, cfg.Interval)
		stop()

		_ = lead.Close()
	}
}
