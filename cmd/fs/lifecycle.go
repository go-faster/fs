package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/lastrun"
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
func runLifecycle(
	ctx context.Context,
	lg *zap.Logger,
	storage fs.Storage,
	cfg LifecycleConfig,
	state lastrun.Store,
) {
	if cfg.Interval <= 0 {
		lg.Warn("Lifecycle enforcement is disabled; rules clients set will be stored but never applied")
		return
	}

	sweeper := &lifecycle.Sweeper{Storage: storage, Log: lg, State: state}
	sweeper.Run(ctx, cfg.Interval)
}
