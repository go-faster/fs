package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/go-faster/fs/storagefs"
)

// firstPassDelay is how long a periodic background pass waits before its first
// run.
//
// Not the interval. A ticker puts the first pass one whole interval away, so a
// node restarted more often than the interval never runs it at all: set
// scrub_interval to 24h, redeploy daily, and the deployment is never scrubbed
// once — with nothing in the log to say so, because the loop is running exactly
// as written.
//
// Not zero either. Scrubbing the instant the process is up would have a
// crashlooping node re-walk every object on each restart, which is load at the
// moment it is least able to carry it. A short delay makes a healthy restart
// scrub promptly and a fast crashloop never get there.
//
// ponytail: a node restarted more often than this still never scrubs. The fix
// is durable state — persist when the last pass ran and run on start when it is
// older than the interval — worth building once a deployment restarts that
// fast.
const firstPassDelay = 5 * time.Minute

// firstPass is how long to wait before the first pass of a loop that thereafter
// runs every interval. A short interval is its own answer: waiting longer than
// the cadence the operator asked for would be its own surprise.
func firstPass(interval time.Duration) time.Duration {
	if interval < firstPassDelay {
		return interval
	}

	return firstPassDelay
}

// runScrubber runs the background integrity scrubber until ctx is canceled,
// logging each pass's result: shortly after startup, and every configured
// interval thereafter. A pass that finds corruption is logged at error level (a
// loud, single-node report).
func runScrubber(ctx context.Context, lg *zap.Logger, storage *storagefs.Storage, cfg IntegrityConfig) {
	scrubLoop(ctx, lg, storage, cfg, firstPass(cfg.ScrubInterval))
}

// scrubLoop is runScrubber with the delay before the first pass supplied, so a
// test can watch that pass happen instead of waiting minutes for it.
func scrubLoop(
	ctx context.Context,
	lg *zap.Logger,
	storage *storagefs.Storage,
	cfg IntegrityConfig,
	first time.Duration,
) {
	if cfg.ScrubInterval <= 0 {
		return
	}

	opts := storagefs.ScrubOptions{Quarantine: cfg.ScrubQuarantine}

	lg.Info("Scrubber started",
		zap.Duration("interval", cfg.ScrubInterval),
		zap.Duration("first_pass", first),
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
