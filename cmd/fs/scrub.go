package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/go-faster/fs/storagefs"
)

// runScrubber runs the background integrity scrubber on the configured cadence
// until ctx is canceled, logging each pass's result. A pass that finds
// corruption is logged at error level (a loud, single-node report).
func runScrubber(ctx context.Context, lg *zap.Logger, storage *storagefs.Storage, cfg IntegrityConfig) {
	if cfg.ScrubInterval <= 0 {
		return
	}

	opts := storagefs.ScrubOptions{Quarantine: cfg.ScrubQuarantine}

	lg.Info("Scrubber started",
		zap.Duration("interval", cfg.ScrubInterval),
		zap.Bool("quarantine", cfg.ScrubQuarantine),
	)

	ticker := time.NewTicker(cfg.ScrubInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scrubOnce(ctx, lg, storage, opts)
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
