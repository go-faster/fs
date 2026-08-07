package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/lastrun"
	"github.com/go-faster/fs/storagefs"
)

// firstPassFloor is the shortest a periodic background pass will ever wait
// before running, however overdue the last-run record says it is.
//
// It is what stops a crashlooping node from re-running an overdue pass on every
// restart: completion is recorded only when the pass finishes, so a node that
// dies part-way through would otherwise start another walk immediately each
// time it came back.
const firstPassFloor = time.Minute

// scrubTask names the scrub's last-run record. The single-node scrubber walks
// this node's own objects, and so does each cluster node's — see
// clusterScrubTask for why the cluster record is per node.
const scrubTask = "scrub"

// runScrubber runs the background integrity scrubber until ctx is canceled,
// logging each pass's result. The first pass is due one interval after the last
// one recorded in state, so restarting neither postpones the scrub by a whole
// interval nor re-runs it on every restart. A pass that finds corruption is
// logged at error level (a loud, single-node report).
func runScrubber(
	ctx context.Context,
	lg *zap.Logger,
	storage *storagefs.Storage,
	cfg IntegrityConfig,
	state lastrun.Store,
) {
	scrubLoop(ctx, lg, storage, cfg, state, firstPassFloor)
}

// scrubLoop is runScrubber with the floor on the first wait supplied, so a test
// can watch that pass happen instead of waiting a minute for it.
func scrubLoop(
	ctx context.Context,
	lg *zap.Logger,
	storage *storagefs.Storage,
	cfg IntegrityConfig,
	state lastrun.Store,
	floor time.Duration,
) {
	if cfg.ScrubInterval <= 0 {
		return
	}

	opts := storagefs.ScrubOptions{Quarantine: cfg.ScrubQuarantine}

	// A state store that cannot be read is not a reason to stop scrubbing: the
	// pass falls back to "due now", which is the same answer a fresh
	// deployment gets.
	last, err := state.LastRun(ctx, scrubTask)
	if err != nil {
		lg.Warn("Last scrub time unreadable; scrubbing as if it is due", zap.Error(err))
	}

	first := lastrun.Due(last, time.Now(), cfg.ScrubInterval, floor)

	lg.Info("Scrubber started",
		zap.Duration("interval", cfg.ScrubInterval),
		zap.Duration("first_pass", first),
		zap.Time("last_scrub", last),
		zap.Bool("quarantine", cfg.ScrubQuarantine),
	)

	timer := time.NewTimer(first)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		timer.Reset(cfg.ScrubInterval)
		scrubOnce(ctx, lg, storage, opts)

		// Recorded after the pass, so an interrupted one is still due.
		if err := state.SetLastRun(ctx, scrubTask, time.Now()); err != nil && ctx.Err() == nil {
			lg.Warn("Could not record the scrub time; a restart will scrub again", zap.Error(err))
		}
	}
}

func scrubOnce(ctx context.Context, lg *zap.Logger, storage *storagefs.Storage, opts storagefs.ScrubOptions) {
	start := time.Now()

	report, err := storage.Scrub(ctx, opts)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}

		lg.Error("Scrub failed", zap.Error(err))

		return
	}

	fields := []zap.Field{
		zap.Int("scanned", report.Scanned),
		zap.Int("ok", report.OK),
		zap.Int("corrupt", len(report.Corrupt)),
		zap.Int("unverifiable", report.Unverifiable),
		zap.Int("quarantined", report.Quarantined),
		zap.Duration("took", time.Since(start)),
	}

	if !report.Healthy() {
		refs := make([]string, len(report.Corrupt))
		for i, r := range report.Corrupt {
			refs[i] = r.Bucket + "/" + r.Key
		}

		lg.Error("Scrub found corruption", append(fields, zap.Strings("objects", refs))...)

		return
	}

	lg.Info("Scrub complete", fields...)
}
