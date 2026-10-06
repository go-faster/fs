package engine

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/table"
	"github.com/go-faster/fs/internal/lastrun"
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
	// OnSweep, when set, is told how each anti-entropy sweep ended: nil
	// once it synced the layout, or why it has not — what an operator
	// needs to see a layout change that does not complete.
	OnSweep func(error)
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
func (e *Engine) Run(ctx context.Context, cfg RunConfig) {
	cfg = cfg.withDefaults()

	var wg sync.WaitGroup

	if cfg.Cluster {
		wg.Go(func() { e.blocks.Run(ctx, cfg.Resync) })
		wg.Go(func() { e.syncLoop(ctx, cfg.Sync, cfg.OnSweep) })
	}

	wg.Go(func() {
		e.gcLoop(ctx, cfg.GC, func() error {
			n, err := e.blocks.GC(ctx, cfg.Grace, e.BlockLive)

			// Rows first: a block's last reference collected this pass leaves
			// it for the block collection after the grace period.
			for _, collect := range []func(context.Context, time.Duration) error{
				e.objects.Collect, e.refs.Collect, e.parts.Collect, e.uploads.Collect,
			} {
				if cerr := collect(ctx, cfg.Tombstones); cerr != nil {
					err = errors.Join(err, cerr)
				}
			}

			e.gcMu.Lock()
			e.gc = gcStats{last: time.Now(), collected: e.gc.collected + int64(n), failed: err != nil}
			e.gcMu.Unlock()

			return err
		})
	})

	wg.Wait()
}

// gcFloor is the shortest wait before a collection after start, so a node
// crashlooping mid-pass does not start another pass on every restart.
var gcFloor = time.Minute

// gcLoop runs pass every period until ctx is canceled. The first pass is
// timed from the last one that completed, recorded in the metadata database:
// a ticker started with the process would never fire on a node restarted more
// often than the period.
func (e *Engine) gcLoop(ctx context.Context, period time.Duration, pass func() error) {
	t := time.NewTimer(lastrun.Due(e.lastGC(), time.Now(), period, gcFloor))
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		t.Reset(period)

		// A failed pass is not recorded, so it is due again after a restart.
		if pass() == nil {
			e.setLastGC(time.Now())
		}
	}
}

var (
	runBucket = []byte("engine")
	lastGCKey = []byte("gc.last")
)

// lastGC returns when collection last completed; zero if never or unreadable,
// which makes it due.
func (e *Engine) lastGC() time.Time {
	var last time.Time

	_ = e.db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket(runBucket); b != nil {
			if v := b.Get(lastGCKey); len(v) == 8 {
				last = time.Unix(0, int64(binary.BigEndian.Uint64(v))) //nolint:gosec // Written from a positive int64.
			}
		}

		return nil
	})

	return last
}

func (e *Engine) setLastGC(t time.Time) {
	_ = e.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(runBucket)
		if err != nil {
			return err
		}

		return b.Put(lastGCKey, binary.BigEndian.AppendUint64(nil, uint64(t.UnixNano()))) //nolint:gosec // Wall clock, positive.
	})
}

// syncRetry is how soon a failed sweep runs again, doubling on each further
// failure up to the sync period.
var syncRetry = 5 * time.Second

// syncLoop runs a sweep every period, and right away when the layout version
// changes — a moved partition should not wait a full period.
func (e *Engine) syncLoop(ctx context.Context, period time.Duration, onSweep func(error)) {
	sweepLoop(ctx, period, e.layoutVersion, e.Sweep, onSweep)
}

// layoutVersion is the adopted layout's version, zero before the first.
func (e *Engine) layoutVersion() uint64 {
	if l := e.member.Layout(); l != nil {
		return l.Version
	}

	return 0
}

// sweepLoop drives sweep: every period, when version moves, and after a
// failure again soon. The first sweep of a new layout often fails for a
// moment — a peer has not adopted the layout yet, or gossip has not carried a
// joining node's address — and until a sweep succeeds this node has not
// synced the version, so older versions stay retained and a removed node
// keeps running. Waiting the full period for the next try would hold the
// whole layout change that long. The retry backs off to the period, so a node
// that stays down costs one sweep per period, as before.
func sweepLoop(
	ctx context.Context,
	period time.Duration,
	version func() uint64,
	sweep func(context.Context) error,
	onSweep func(error),
) {
	var (
		last  uint64
		retry time.Duration
		due   time.Time
	)

	tick := time.NewTicker(period)
	defer tick.Stop()

	check := time.NewTicker(time.Second)
	defer check.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-check.C:
			moved := version() != 0 && version() != last
			if !moved && (due.IsZero() || time.Now().Before(due)) {
				continue
			}
		}

		// A new layout starts its own backoff: an old one's failures say
		// nothing about it.
		if v := version(); v != last {
			last, retry = v, 0
		}

		err := sweep(ctx)
		if onSweep != nil && ctx.Err() == nil {
			onSweep(err)
		}

		if err == nil {
			retry, due = 0, time.Time{}

			continue
		}

		retry = min(max(retry*2, syncRetry), period)
		due = time.Now().Add(retry)
	}
}

// Sweep runs anti-entropy over every table and the blocks, and shard repair.
// When all of it completes under one layout version — every replica compared
// with, everything that moved handed over — this node has synced that
// version, and says so through gossip: once every node has, the versions
// before it retire.
func (e *Engine) Sweep(ctx context.Context) error {
	l := e.member.Layout()
	if l == nil {
		return table.ErrNoLayout
	}

	var errs []error

	for _, sync := range []func(context.Context) error{
		e.buckets.Sync, e.objects.Sync, e.refs.Sync, e.parts.Sync, e.uploads.Sync, e.keys.Sync, e.blocks.Sync,
	} {
		if err := sync(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	if err := e.blocks.RepairShards(ctx, e.BlockLive); err != nil {
		errs = append(errs, err)
	} else if n := e.blocks.ShardStats().Unreachable; n > 0 {
		errs = append(errs, errors.Errorf("shard repair: %d partitions unreachable", n))
	}

	if err := errors.Join(errs...); err != nil {
		return err
	}

	if cur := e.member.Layout(); cur == nil || cur.Version != l.Version {
		return errors.New("layout changed during the sweep")
	}

	return e.member.MarkSynced(l.Version)
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
	// Shards describe erasure-coded blocks: how many are short of shards,
	// and the repair that rebuilds them.
	Shards    block.ShardStats
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
		Shards:    e.blocks.ShardStats(),
		TableSync: map[string]table.SyncStats{
			"buckets":     e.buckets.SyncStats(),
			"objects":     e.objects.SyncStats(),
			"block_refs":  e.refs.SyncStats(),
			"parts":       e.parts.SyncStats(),
			"uploads":     e.uploads.SyncStats(),
			"access_keys": e.keys.SyncStats(),
		},
		Tombstones: map[string]table.GCStats{
			"objects":    e.objects.GCStats(),
			"block_refs": e.refs.GCStats(),
			"parts":      e.parts.GCStats(),
			"uploads":    e.uploads.GCStats(),
		},
		LastGC:   gc.last,
		GCFailed: gc.failed,
	}
}
