package main

import (
	"context"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/go-faster/fs/internal/adminhandler"
)

// RegisterMetrics exports the ROADMAP Phase 9 cluster metrics: per-disk
// capacity across the whole topology (every node reports its usage into the
// registry), placement skew, this node's repair backlog and scrub totals, and
// its rebalance runner progress.
func (rt *clusterRuntime) RegisterMetrics(provider metric.MeterProvider) error {
	meter := provider.Meter("go-faster/fs/cluster")

	const unitObjects = "{object}"

	var (
		diskTotal   metric.Int64ObservableGauge
		diskFree    metric.Int64ObservableGauge
		diskFull    metric.Float64ObservableGauge
		skew        metric.Float64ObservableGauge
		nodes       metric.Int64ObservableGauge
		disks       metric.Int64ObservableGauge
		queueDepth  metric.Int64ObservableGauge
		rebActive   metric.Int64ObservableGauge
		rebObjects  metric.Int64ObservableGauge
		rebMoved    metric.Int64ObservableGauge
		rebFailed   metric.Int64ObservableGauge
		scrubPasses metric.Int64ObservableCounter
		scrubObj    metric.Int64ObservableCounter
		scrubRep    metric.Int64ObservableCounter
		scrubFail   metric.Int64ObservableCounter
		rebuilt     metric.Int64ObservableCounter
		swept       metric.Int64ObservableCounter
		corrupt     metric.Int64ObservableCounter
		converted   metric.Int64ObservableCounter
		ecUnverif   metric.Int64ObservableGauge
	)

	instruments := []struct {
		target *metric.Int64ObservableGauge
		name   string
		desc   string
		unit   string
	}{
		{&diskTotal, "fs.cluster.disk.total_bytes", "Reported filesystem size of a cluster disk.", "By"},
		{&diskFree, "fs.cluster.disk.free_bytes", "Reported free space of a cluster disk.", "By"},
		{&nodes, "fs.cluster.nodes", "Cluster members in the current topology.", "{node}"},
		{&disks, "fs.cluster.disks", "Disks in the current topology.", "{disk}"},
		{&queueDepth, "fs.cluster.repair.queue_depth", "Objects with pending async replication/repair work on this node.", unitObjects},
		{&rebActive, "fs.cluster.rebalance.active", "1 while this node's rebalance runner is waiting or running.", "1"},
		{&rebObjects, "fs.cluster.rebalance.objects", "Objects processed by this node's current or last rebalance run.", unitObjects},
		{&rebMoved, "fs.cluster.rebalance.relocated", "Objects changed by this node's current or last rebalance run.", unitObjects},
		{&rebFailed, "fs.cluster.rebalance.failed", "Objects that failed in this node's current or last rebalance run.", unitObjects},
		{&ecUnverif, "fs.cluster.scrub.ec_unverified", "1 when the last scrub pass saw an EC set failing parity verification.", "1"},
	}

	for _, ins := range instruments {
		g, err := meter.Int64ObservableGauge(ins.name, metric.WithDescription(ins.desc), metric.WithUnit(ins.unit))
		if err != nil {
			return errors.Wrapf(err, "instrument %s", ins.name)
		}

		*ins.target = g
	}

	counters := []struct {
		target *metric.Int64ObservableCounter
		name   string
		desc   string
	}{
		{&scrubPasses, "fs.cluster.scrub.passes", "Completed scrub passes on this node."},
		{&scrubObj, "fs.cluster.scrub.objects", "Objects fed through scrub repair on this node."},
		{&scrubRep, "fs.cluster.scrub.repaired", "Objects a scrub pass changed."},
		{&scrubFail, "fs.cluster.scrub.failed", "Objects whose scrub repair errored."},
		{&rebuilt, "fs.cluster.repair.rebuilt_fragments", "Fragments rebuilt by scrubs on this node (repair lag indicator)."},
		{&swept, "fs.cluster.repair.swept_stale", "Stale names retired by scrubs on this node."},
		{&corrupt, "fs.cluster.repair.corrupt_replicas", "Replica payloads that failed checksum verification (bit-rot)."},
		{&converted, "fs.cluster.repair.converted_objects", "Objects rewritten to their bucket's current scheme."},
	}

	for _, ins := range counters {
		c, err := meter.Int64ObservableCounter(ins.name, metric.WithDescription(ins.desc))
		if err != nil {
			return errors.Wrapf(err, "instrument %s", ins.name)
		}

		*ins.target = c
	}

	var err error

	diskFull, err = meter.Float64ObservableGauge("fs.cluster.disk.fullness",
		metric.WithDescription("Used fraction of a cluster disk (0..1); only disks reporting capacity."), metric.WithUnit("1"))
	if err != nil {
		return errors.Wrap(err, "instrument fs.cluster.disk.fullness")
	}

	skew, err = meter.Float64ObservableGauge("fs.cluster.placement.skew",
		metric.WithDescription("Max minus min disk fullness across disks reporting capacity — the capacity-imbalance watermark input."), metric.WithUnit("1"))
	if err != nil {
		return errors.Wrap(err, "instrument fs.cluster.placement.skew")
	}

	all := make([]metric.Observable, 0, len(instruments)+len(counters)+2)
	for _, ins := range instruments {
		all = append(all, *ins.target)
	}

	for _, ins := range counters {
		all = append(all, *ins.target)
	}

	all = append(all, diskFull, skew)

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		topo := rt.coord.Topology()

		var (
			minFull = 2.0
			maxFull = -1.0
		)

		for i := range topo.Nodes {
			n := &topo.Nodes[i]

			for _, d := range n.Disks {
				attrs := metric.WithAttributes(
					attribute.String("node", string(n.ID)),
					attribute.String("disk", string(d.ID)),
					attribute.String("rack", n.FailureDomain()),
				)

				if d.TotalBytes == 0 {
					continue // Capacity unknown (old node, unreadable filesystem).
				}

				full := 1 - float64(d.FreeBytes)/float64(d.TotalBytes)

				o.ObserveInt64(diskTotal, int64(min(d.TotalBytes, 1<<62)), attrs) //nolint:gosec // Clamped.
				o.ObserveInt64(diskFree, int64(min(d.FreeBytes, 1<<62)), attrs)   //nolint:gosec // Clamped.
				o.ObserveFloat64(diskFull, full, attrs)

				minFull = min(minFull, full)
				maxFull = max(maxFull, full)
			}
		}

		if maxFull >= minFull {
			o.ObserveFloat64(skew, maxFull-minFull)
		}

		o.ObserveInt64(nodes, int64(len(topo.Nodes)))
		o.ObserveInt64(disks, int64(topo.DiskCount()))
		o.ObserveInt64(queueDepth, int64(rt.coord.QueueDepth()))

		st := rt.rebalance.Status(ctx)

		active := int64(0)
		if st.State == adminhandler.RebalanceWaiting || st.State == adminhandler.RebalanceRunning {
			active = 1
		}

		o.ObserveInt64(rebActive, active)
		o.ObserveInt64(rebObjects, int64(st.Objects))
		o.ObserveInt64(rebMoved, int64(st.Relocated))
		o.ObserveInt64(rebFailed, int64(st.Failed))

		o.ObserveInt64(scrubPasses, rt.scrub.passes.Load())
		o.ObserveInt64(scrubObj, rt.scrub.objects.Load())
		o.ObserveInt64(scrubRep, rt.scrub.repaired.Load())
		o.ObserveInt64(scrubFail, rt.scrub.failed.Load())
		o.ObserveInt64(rebuilt, rt.scrub.rebuilt.Load())
		o.ObserveInt64(swept, rt.scrub.sweptStale.Load())
		o.ObserveInt64(corrupt, rt.scrub.corrupt.Load())
		o.ObserveInt64(converted, rt.scrub.converted.Load())
		o.ObserveInt64(ecUnverif, rt.scrub.ecUnverifiedLastScrub.Load())

		return nil
	}, all...)
	if err != nil {
		return errors.Wrap(err, "register cluster metrics callback")
	}

	return nil
}
