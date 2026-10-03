package engine

import (
	"context"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/table"
)

// RunConfig sets how often the engine's background work runs. Zero values
// take the defaults.
type RunConfig struct {
	// Cluster runs the work that only makes sense with peers: anti-entropy
	// and the block resync queue. A single node has nothing to compare with.
	Cluster bool
	// Sync is the anti-entropy period (default 10 minutes); a layout change
	// also starts a sweep right away.
	Sync time.Duration
	// Resync is how often queued block copies are retried (default 10s).
	Resync time.Duration
	// GC is the block collection period (default an hour), and Grace how
	// long an unreferenced block is kept (default block.DefaultGrace).
	GC    time.Duration
	Grace time.Duration
	// Tombstones is how long a deleted row is kept before it is collected
	// (default a day): long past any write still in flight to a replica,
	// and any partition handover a layout change started.
	Tombstones time.Duration
}

func (c RunConfig) withDefaults() RunConfig {
	if c.Sync <= 0 {
		c.Sync = 10 * time.Minute
	}

	if c.Resync <= 0 {
		c.Resync = 10 * time.Second
	}

	if c.GC <= 0 {
		c.GC = time.Hour
	}

	if c.Grace <= 0 {
		c.Grace = block.DefaultGrace
	}

	if c.Tombstones <= 0 {
		c.Tombstones = 24 * time.Hour
	}

	return c
}

// Run does the engine's background work until ctx is canceled.
//
// ponytail: a failed sweep or collection is retried next period, and
// visible in Stats; nothing is logged here — the library stays quiet.
func (e *Engine) Run(ctx context.Context, cfg RunConfig) {
	cfg = cfg.withDefaults()

	var wg sync.WaitGroup

	if cfg.Cluster {
		wg.Go(func() { e.buckets.Run(ctx, cfg.Sync) })
		wg.Go(func() { e.objects.Run(ctx, cfg.Sync) })
		wg.Go(func() { e.refs.Run(ctx, cfg.Sync) })
		wg.Go(func() { e.parts.Run(ctx, cfg.Sync) })
		wg.Go(func() { e.blocks.Run(ctx, cfg.Resync) })
		wg.Go(func() {
			every(ctx, cfg.Sync, func() { _ = e.blocks.Sync(ctx) })
		})
	}

	wg.Go(func() {
		every(ctx, cfg.GC, func() {
			n, err := e.blocks.GC(ctx, cfg.Grace, e.BlockLive)

			// Rows first: a block's last reference collected this pass leaves
			// it for the block collection after the grace period.
			for _, collect := range []func(context.Context, time.Duration) error{
				e.objects.Collect, e.refs.Collect, e.parts.Collect,
			} {
				if cerr := collect(ctx, cfg.Tombstones); cerr != nil {
					err = errors.Join(err, cerr)
				}
			}

			e.gcMu.Lock()
			e.gc = gcStats{last: time.Now(), collected: e.gc.collected + int64(n), failed: err != nil}
			e.gcMu.Unlock()
		})
	})

	wg.Wait()
}

// every calls fn every period until ctx is canceled.
func every(ctx context.Context, period time.Duration, fn func()) {
	t := time.NewTicker(period)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

type gcStats struct {
	last      time.Time
	collected int64
	failed    bool
}

// Stats are the engine's counters, for metrics and the admin API.
type Stats struct {
	Blocks block.Stats
	// BlockSync and TableSync describe the last anti-entropy sweeps; zero on
	// a single node.
	BlockSync block.SyncStats
	TableSync map[string]table.SyncStats
	// Tombstones describe deleted rows waiting for, and removed by,
	// collection, by table.
	Tombstones map[string]table.GCStats
	// LastGC is when block collection last finished; zero before the first.
	LastGC   time.Time
	GCFailed bool
}

// Stats returns the engine's counters.
func (e *Engine) Stats() Stats {
	e.gcMu.Lock()
	gc := e.gc
	e.gcMu.Unlock()

	return Stats{
		Blocks:    e.blocks.Stats(),
		BlockSync: e.blocks.SyncStats(),
		TableSync: map[string]table.SyncStats{
			"buckets":    e.buckets.SyncStats(),
			"objects":    e.objects.SyncStats(),
			"block_refs": e.refs.SyncStats(),
			"parts":      e.parts.SyncStats(),
		},
		Tombstones: map[string]table.GCStats{
			"objects":    e.objects.GCStats(),
			"block_refs": e.refs.GCStats(),
			"parts":      e.parts.GCStats(),
		},
		LastGC:   gc.last,
		GCFailed: gc.failed,
	}
}
