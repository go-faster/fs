package main

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/cluster/peer"
)

// registerReplicationMetrics exports each replicating bucket's backlog: the
// versions some target has yet to receive, and their bytes — what losing
// this deployment would lose — and, as a target, the versions received and
// refused. A bucket is reported by the node elected to
// run its replication, so a cluster does not report it once per node.
// member is nil for a single node.
func registerReplicationMetrics(mp metric.MeterProvider, e *engine.Engine, member *peer.Member) error {
	meter := mp.Meter("github.com/go-faster/fs/replication")

	backlog, err := meter.Int64ObservableGauge("fs.replication.backlog",
		metric.WithUnit("{version}"),
		metric.WithDescription("Versions some replication target has yet to receive, by bucket."))
	if err != nil {
		return errors.Wrap(err, "gauge fs.replication.backlog")
	}

	size, err := meter.Int64ObservableGauge("fs.replication.backlog.size",
		metric.WithUnit("By"),
		metric.WithDescription("Bytes of the versions some replication target has yet to receive, by bucket."))
	if err != nil {
		return errors.Wrap(err, "gauge fs.replication.backlog.size")
	}

	applied, err := meter.Int64ObservableCounter("fs.replication.applied",
		metric.WithUnit("{version}"),
		metric.WithDescription("Versions received from other deployments and applied here since start."))
	if err != nil {
		return errors.Wrap(err, "counter fs.replication.applied")
	}

	refused, err := meter.Int64ObservableCounter("fs.replication.refused",
		metric.WithUnit("{version}"),
		metric.WithDescription("Versions received from other deployments and refused since start; the source retries them."))
	if err != nil {
		return errors.Wrap(err, "counter fs.replication.refused")
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		st := e.ReplicaStats()
		o.ObserveInt64(applied, st.Applied)
		o.ObserveInt64(refused, st.Refused)

		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		// Unreadable, the backlog is left out of this collection rather
		// than reported as zero: an empty queue is the one wrong answer.
		all, err := e.ReplicationBacklogs(ctx)
		if err != nil {
			return nil //nolint:nilerr // A failed read must not fail the other metrics.
		}

		for bucket, u := range all {
			if member != nil && !sweepsBucket(member, bucket) {
				continue
			}

			attr := metric.WithAttributes(attribute.String("bucket", bucket))
			o.ObserveInt64(backlog, u.Objects, attr)
			o.ObserveInt64(size, u.Bytes, attr)
		}

		return nil
	}, backlog, size, applied, refused)
	if err != nil {
		return errors.Wrap(err, "register replication metrics")
	}

	return nil
}
