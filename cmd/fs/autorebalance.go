package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// Auto-rebalance policy defaults (ROADMAP Phase 9): the settle window is the
// hysteresis — the membership must hold still that long before data moves
// (a rolling restart or a flapping node must not trigger walk storms) — and
// the cooldown is the minimum gap between this node's trigger attempts.
const (
	defaultRebalanceSettle   = time.Minute
	defaultRebalanceCooldown = 15 * time.Minute
	autoRebalancePoll        = 5 * time.Second
)

// rebalancePolicy drives the Phase 8 rebalance engine automatically: when the
// placement-relevant membership (nodes, racks, disks, weights) differs from
// what the last completed rebalance covered and has been stable for the
// settle window, it starts the node's rebalance controller. Every node runs
// the policy; the etcd election keeps a single walker and the shared
// "applied" signature keeps converged clusters quiet. Manual wins: an
// operator-paused runner is never resumed, and an operator-started run simply
// counts as the walk in progress.
type rebalancePolicy struct {
	lg       *zap.Logger
	ctl      *rebalanceController
	settle   time.Duration
	cooldown time.Duration

	// pendingSig / stableSince track the hysteresis window.
	pendingSig  string
	stableSince time.Time
	// appliedSig is the last signature this node knows a completed walk
	// covered (mirrors the etcd applied key).
	appliedSig string
	// lastAttempt stamps this node's last trigger, for the cooldown.
	lastAttempt time.Time
}

// RunAutoRebalancer runs the policy loop until ctx is canceled. A no-op when
// disabled in config.
func (rt *clusterRuntime) RunAutoRebalancer(ctx context.Context, cfg RebalanceConfig) {
	if cfg.AutoDisabled {
		rt.lg.Info("Auto rebalancing disabled")
		return
	}

	settle := cfg.Settle
	if settle <= 0 {
		settle = defaultRebalanceSettle
	}

	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = defaultRebalanceCooldown
	}

	p := &rebalancePolicy{
		lg:       rt.lg,
		ctl:      rt.rebalance,
		settle:   settle,
		cooldown: cooldown,
		// The boot membership is the baseline until etcd says otherwise: a
		// node (re)starting into a steady cluster must not trigger a walk.
		appliedSig: rt.coord.Topology().Signature(),
	}

	rt.lg.Info("Auto rebalancing enabled",
		zap.Duration("settle", settle),
		zap.Duration("cooldown", cooldown),
	)

	ticker := time.NewTicker(autoRebalancePoll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		p.tick(ctx, time.Now())
	}
}

// tick evaluates the policy once.
func (p *rebalancePolicy) tick(ctx context.Context, now time.Time) {
	sig := p.ctl.coord.Topology().Signature()

	// Hysteresis: (re)arm the settle window on every membership change.
	if sig != p.pendingSig {
		p.pendingSig = sig
		p.stableSince = now
	}

	if sig == p.appliedSig {
		return // Converged as far as this node knows.
	}

	if now.Sub(p.stableSince) < p.settle {
		return // Still settling.
	}

	// The cluster-wide applied signature may already cover this membership
	// (another node walked it).
	if applied, ok, err := etcd.LoadRebalanceApplied(ctx, p.ctl.client, p.ctl.etcdCfg); err == nil && ok {
		if p.appliedSig != applied {
			p.appliedSig = applied
		}

		if applied == sig {
			return
		}
	}

	if now.Sub(p.lastAttempt) < p.cooldown {
		return
	}

	// Manual wins: never resume an operator pause, never stack on a run in
	// progress on this node.
	switch p.ctl.Status(ctx).State {
	case adminhandler.RebalanceWaiting, adminhandler.RebalanceRunning, adminhandler.RebalancePaused:
		return
	case adminhandler.RebalanceIdle, adminhandler.RebalanceDone, adminhandler.RebalanceFailed:
	}

	// If some runner already holds the cluster-wide slot (a manual CLI run,
	// another node's auto run), stay out of its way instead of queueing as a
	// standby walker.
	if held, err := etcd.RebalanceLeaderExists(ctx, p.ctl.client, p.ctl.etcdCfg); err != nil || held {
		return
	}

	p.lastAttempt = now

	p.lg.Info("Membership changed and settled; starting automatic rebalance",
		zap.String("signature", sig),
		zap.Duration("settle", p.settle),
	)

	if err := p.ctl.Start(ctx); err != nil {
		p.lg.Warn("Automatic rebalance start refused", zap.Error(err))
	}
}
