package main

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/cluster/peer"
	"github.com/go-faster/fs/internal/replication"
)

// newReplicator builds the geo-replication runner. In a cluster each bucket
// is shipped by the node elected for it as for its lifecycle sweep; member
// is nil for a single node, which ships every bucket.
func newReplicator(lg *zap.Logger, e *engine.Engine, member *peer.Member) *replication.Runner {
	r := &replication.Runner{Source: e, Log: lg.Named("replication")}
	if member != nil {
		r.Owns = func(bucket string) bool { return sweepsBucket(member, bucket) }
	}

	return r
}

// registerReplicationMetrics exports geo-replication. As a source, by
// bucket: the backlog, the versions some target has yet to receive and their
// bytes — what losing this deployment would lose — and by target: the lag,
// deliveries and refusals, the blocks offered and sent, whether the target
// is failing, and what its backfill has left. A bucket is reported by the node that runs its
// replication, so a cluster does not report it once per node. As a target:
// the versions received and refused.
func registerReplicationMetrics(mp metric.MeterProvider, e *engine.Engine, r *replication.Runner) error {
	meter := mp.Meter("github.com/go-faster/fs/replication")

	var (
		insts []metric.Observable
		err   error
	)

	gauge := func(name, unit, desc string) metric.Int64ObservableGauge {
		if err != nil {
			return nil
		}

		g, gerr := meter.Int64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(desc))
		if gerr != nil {
			err = errors.Wrapf(gerr, "gauge %s", name)
		}

		insts = append(insts, g)

		return g
	}
	counter := func(name, unit, desc string) metric.Int64ObservableCounter {
		if err != nil {
			return nil
		}

		c, cerr := meter.Int64ObservableCounter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		if cerr != nil {
			err = errors.Wrapf(cerr, "counter %s", name)
		}

		insts = append(insts, c)

		return c
	}

	backlog := gauge("fs.replication.backlog", "{version}",
		"Versions some replication target has yet to receive, by bucket.")
	size := gauge("fs.replication.backlog.size", "By",
		"Bytes of the versions some replication target has yet to receive, by bucket.")
	failing := gauge("fs.replication.failing", "1",
		"1 while requests to a replication target fail and are retried with backoff, by bucket and target.")
	shipped := counter("fs.replication.shipped", "{version}",
		"Versions delivered to a replication target since start, by bucket and target.")
	failed := counter("fs.replication.failed", "{version}",
		"Versions a replication target refused since start, by bucket and target; retried every minute.")
	offered := counter("fs.replication.blocks.offered", "{block}",
		"Blocks of versions shipped to a replication target since start; against blocks.sent, the dedupe.")
	sent := counter("fs.replication.blocks.sent", "{block}",
		"Blocks a replication target lacked and was sent since start.")
	bytesSent := counter("fs.replication.sent", "By",
		"Bytes of blocks sent to a replication target since start.")
	remaining := gauge("fs.replication.backfill.remaining", "{version}",
		"Estimated versions a replication target's backfill has not reached yet, by bucket and target; 0 when none runs.")
	applied := counter("fs.replication.applied", "{version}",
		"Versions received from other deployments and applied here since start.")
	refused := counter("fs.replication.refused", "{version}",
		"Versions received from other deployments and refused since start; the source retries them.")

	if err != nil {
		return err
	}

	lag, err := meter.Float64ObservableGauge("fs.replication.lag",
		metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest change still to ship to a replication target at the last pass: "+
			"the recovery point a site loss would have, by bucket and target."))
	if err != nil {
		return errors.Wrap(err, "gauge fs.replication.lag")
	}

	insts = append(insts, lag)

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		st := e.ReplicaStats()
		o.ObserveInt64(applied, st.Applied)
		o.ObserveInt64(refused, st.Refused)

		for t, ts := range r.Stats() {
			attr := metric.WithAttributes(attribute.String("bucket", t.Bucket), attribute.String("target", t.Name))
			o.ObserveFloat64(lag, ts.Lag.Seconds(), attr)
			o.ObserveInt64(shipped, ts.Shipped, attr)
			o.ObserveInt64(failed, ts.Refused, attr)
			o.ObserveInt64(offered, ts.BlocksOffered, attr)
			o.ObserveInt64(sent, ts.BlocksSent, attr)
			o.ObserveInt64(bytesSent, ts.BytesSent, attr)

			var f int64
			if ts.Failing {
				f = 1
			}

			o.ObserveInt64(failing, f, attr)
			o.ObserveInt64(remaining, ts.BackfillRemaining, attr)
		}

		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		// Unreadable, the backlog is left out of this collection rather
		// than reported as zero: an empty queue is the one wrong answer.
		all, err := e.ReplicationBacklogs(ctx)
		if err != nil {
			return nil //nolint:nilerr // A failed read must not fail the other metrics.
		}

		for bucket, u := range all {
			if !r.Runs(bucket) {
				continue
			}

			attr := metric.WithAttributes(attribute.String("bucket", bucket))
			o.ObserveInt64(backlog, u.Objects, attr)
			o.ObserveInt64(size, u.Bytes, attr)
		}

		return nil
	}, insts...)
	if err != nil {
		return errors.Wrap(err, "register replication metrics")
	}

	return nil
}
