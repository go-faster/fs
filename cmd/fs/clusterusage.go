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

// usageFlushInterval is how often accumulated per-bucket deltas are pushed to
// etcd. Batching is the point: a bucket under load takes one round trip per
// interval instead of one per object, and the write path never waits on the
// control plane to acknowledge an object it has already made durable.
const usageFlushInterval = time.Second

// usageRecountInterval is how often the cluster re-derives every bucket's
// totals from the objects themselves. Deltas keep the record close between
// recounts; the recount is what makes it true again after the ways a delta can
// be lost — a node dying between the commit and the flush, a partial delete, a
// write that raced the previous recount.
const usageRecountInterval = 6 * time.Hour

// usageRecountTimeout bounds one recount pass. It reads every sidecar in the
// cluster, so it is generous; a pass cut short changes nothing, since the next
// one starts over from what is on disk.
const usageRecountTimeout = 30 * time.Minute

// bucketDelta is one bucket's unflushed accounting.
type bucketDelta struct {
	objects int64
	bytes   int64
}

// usageReporter batches per-bucket object accounting and flushes it to the etcd
// control plane.
//
// Observe is called on the write path, so it does no I/O: it folds the delta
// into a map and returns. The flush loop drains the map on an interval, and a
// failed flush puts the delta back rather than dropping it — a bucket's
// accounting is a running total, so a lost batch is lost permanently, while a
// re-added one merely arrives late.
type usageReporter struct {
	client *clientv3.Client
	cfg    etcd.Config
	lg     *zap.Logger

	mu      sync.Mutex
	pending map[string]bucketDelta
}

var _ clusterstore.UsageObserver = (*usageReporter)(nil)

// newUsageReporter builds the reporter a coordinator hands its deltas to.
func newUsageReporter(client *clientv3.Client, cfg etcd.Config, lg *zap.Logger) *usageReporter {
	return &usageReporter{
		client:  client,
		cfg:     cfg,
		lg:      lg,
		pending: make(map[string]bucketDelta),
	}
}

// Observe implements clusterstore.UsageObserver.
func (r *usageReporter) Observe(bucket string, objects, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	d := r.pending[bucket]
	d.objects += objects
	d.bytes += bytes

	// A delta that cancels out is not worth a round trip: an overwrite of the
	// same size, or a create and delete inside one interval.
	if d.objects == 0 && d.bytes == 0 {
		delete(r.pending, bucket)

		return
	}

	r.pending[bucket] = d
}

// take removes and returns the pending deltas.
func (r *usageReporter) take() map[string]bucketDelta {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.pending) == 0 {
		return nil
	}

	out := r.pending
	r.pending = make(map[string]bucketDelta, len(out))

	return out
}

// restore puts an unflushed delta back, folding it into anything that accrued
// meanwhile.
func (r *usageReporter) restore(bucket string, d bucketDelta) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur := r.pending[bucket]
	cur.objects += d.objects
	cur.bytes += d.bytes
	r.pending[bucket] = cur
}

// Flush pushes every pending delta to etcd, keeping what it could not push.
func (r *usageReporter) Flush(ctx context.Context) error {
	var firstErr error

	for bucket, d := range r.take() {
		if err := etcd.AddBucketUsage(ctx, r.client, r.cfg, bucket, d.objects, d.bytes); err != nil {
			r.restore(bucket, d)

			if firstErr == nil {
				firstErr = errors.Wrapf(err, "flush usage for bucket %q", bucket)
			}
		}
	}

	return firstErr
}

// Run flushes on an interval until ctx is done, then flushes once more so an
// orderly shutdown does not strand the last interval's writes.
func (r *usageReporter) Run(ctx context.Context) {
	ticker := time.NewTicker(usageFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The parent context is already done, so the final flush runs on a
			// detached copy of it — same values, its own deadline — or it would
			// be canceled before it started.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageFlushInterval)
			if err := r.Flush(flushCtx); err != nil {
				r.lg.Warn("Final bucket usage flush failed; the next recount corrects it", zap.Error(err))
			}

			cancel()

			return
		case <-ticker.C:
		}

		if err := r.Flush(ctx); err != nil && ctx.Err() == nil {
			r.lg.Debug("Bucket usage flush failed; deltas kept for the next attempt", zap.Error(err))
		}
	}
}

// RunUsageRecount keeps one node re-deriving every bucket's totals from the
// objects themselves.
//
// It campaigns for a cluster-wide slot first: the recount reads every sidecar
// on every disk, so running it on each node would multiply that cost by the
// node count for identical results. A node that loses the election stands by
// and takes over when the holder's lease expires.
func (rt *clusterRuntime) RunUsageRecount(ctx context.Context) {
	if rt.usage == nil {
		return
	}

	for ctx.Err() == nil {
		lead, err := etcd.CampaignUsage(ctx, rt.client, rt.etcdCfg, string(rt.nodeID))
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			rt.lg.Warn("Usage recount election failed", zap.Error(err))

			if !sleepCtx(ctx, usageFlushInterval) {
				return
			}

			continue
		}

		rt.lg.Debug("Holding the usage recount leadership")

		rt.recountLoop(ctx, lead)

		_ = lead.Close()
	}
}

// recountLoop recounts on an interval for as long as leadership is held.
func (rt *clusterRuntime) recountLoop(ctx context.Context, lead *etcd.UsageLeadership) {
	ticker := time.NewTicker(usageRecountInterval)
	defer ticker.Stop()

	for {
		if err := rt.recountUsage(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}

			rt.lg.Warn("Bucket usage recount failed; counters stay on their deltas", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-lead.Done():
			// Lost the lease: another node is recounting now.
			return
		case <-ticker.C:
		}
	}
}

// recountUsage re-derives every bucket's totals and stores them, then removes
// records for buckets that no longer exist.
func (rt *clusterRuntime) recountUsage(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, usageRecountTimeout)
	defer cancel()

	started := time.Now()

	// What each record read before the walk, so deltas applied during it are
	// carried forward instead of being overwritten by a total that predates
	// them.
	before, err := etcd.ListBucketUsage(ctx, rt.client, rt.etcdCfg)
	if err != nil {
		return err
	}

	base := make(map[string]etcd.BucketUsage, len(before))
	for _, rec := range before {
		base[rec.Bucket] = rec
	}

	totals, err := rt.coord.CountObjects(ctx)
	if err != nil {
		return errors.Wrap(err, "count objects")
	}

	buckets, err := rt.coord.ListBuckets(ctx)
	if err != nil {
		return errors.Wrap(err, "list buckets")
	}

	// An existing bucket with no objects is a real answer — zero — so it gets a
	// record too, rather than reading as "never counted".
	for _, b := range buckets {
		if _, ok := totals[b.Name]; !ok {
			totals[b.Name] = clusterstore.BucketTotals{}
		}
	}

	for bucket, t := range totals {
		if err := etcd.SetBucketUsage(ctx, rt.client, rt.etcdCfg, bucket, t.Objects, t.Bytes, base[bucket]); err != nil {
			return err
		}
	}

	// Drop records for buckets that are gone. Deleting a bucket requires it to
	// be empty, so nothing is lost — but the record would otherwise outlive it
	// and keep appearing in the usage listing.
	for _, rec := range before {
		if _, ok := totals[rec.Bucket]; ok {
			continue
		}

		if err := etcd.DeleteBucketUsage(ctx, rt.client, rt.etcdCfg, rec.Bucket); err != nil {
			return err
		}
	}

	rt.lg.Info("Bucket usage recount complete",
		zap.Int("buckets", len(totals)),
		zap.Duration("took", time.Since(started)),
	)

	return nil
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// bucketUsageSource reads the durable per-bucket accounting for the admin API.
// It is a straight read of the control plane: the counters are maintained by
// the write path and the recount, never derived at request time, which is the
// whole point of keeping them.
type bucketUsageSource struct {
	client *clientv3.Client
	cfg    etcd.Config
}

var _ adminhandler.BucketUsageSource = (*bucketUsageSource)(nil)

// newBucketUsageSource builds the admin API's view of the usage index.
func newBucketUsageSource(client *clientv3.Client, cfg etcd.Config) *bucketUsageSource {
	return &bucketUsageSource{client: client, cfg: cfg}
}

// BucketUsage implements adminhandler.BucketUsageSource.
func (s *bucketUsageSource) BucketUsage(ctx context.Context) ([]adminhandler.BucketUsage, error) {
	records, err := etcd.ListBucketUsage(ctx, s.client, s.cfg)
	if err != nil {
		return nil, err
	}

	out := make([]adminhandler.BucketUsage, 0, len(records))
	for _, rec := range records {
		out = append(out, adminhandler.BucketUsage{
			Bucket:  rec.Bucket,
			Objects: rec.Objects,
			Bytes:   rec.Bytes,
			Updated: rec.Updated,
			Counted: rec.Counted,
		})
	}

	return out, nil
}
