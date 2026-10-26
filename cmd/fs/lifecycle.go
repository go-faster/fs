package main

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
	"github.com/go-faster/fs/internal/lastrun"
	"github.com/go-faster/fs/internal/lifecycle"
)

// newSweeper builds the lifecycle sweeper. member is the cluster membership,
// nil for a single node.
func newSweeper(lg *zap.Logger, storage fs.Storage, state lastrun.Store, member *peer.Member) *lifecycle.Sweeper {
	sweeper := &lifecycle.Sweeper{Storage: storage, Log: lg, State: state}
	if member != nil {
		sweeper.Owns = func(bucket string) bool { return sweepsBucket(member, bucket) }
	}

	return sweeper
}

// runLifecycle enforces bucket lifecycle rules until ctx is canceled.
//
// A disabled sweep is reported at warning level rather than passed over in
// silence. The ?lifecycle subresource still accepts rules — it is the same
// bucket metadata either way — so an operator who turns enforcement off has a
// server that stores expiry rules and deletes nothing, and that has to be
// visible in the log rather than discovered from objects that never went away.
func runLifecycle(ctx context.Context, lg *zap.Logger, sweeper *lifecycle.Sweeper, cfg LifecycleConfig) {
	if cfg.Interval <= 0 {
		lg.Warn("Lifecycle enforcement is disabled; rules clients set will be stored but never applied")
		return
	}

	if cfg.ClockOffset != 0 {
		lg.Warn("Lifecycle clock moved; rules will expire objects early by the offset (tests only)",
			zap.Duration("offset", cfg.ClockOffset))

		sweeper.Now = func() time.Time { return time.Now().Add(cfg.ClockOffset) }
	}

	sweeper.Run(ctx, cfg.Interval)
}

// registerLifecycleMetrics exports the sweep: how long ago a pass last
// completed, which is the number that goes wrong first, what the passes
// removed, and the buckets the last one could not finish.
func registerLifecycleMetrics(mp metric.MeterProvider, sweeper *lifecycle.Sweeper) error {
	meter := mp.Meter("github.com/go-faster/fs/internal/lifecycle")

	gauges := map[string]string{
		"fs.lifecycle.age":            "Seconds since a lifecycle pass last completed; -1 when none has.",
		"fs.lifecycle.removed":        "What lifecycle passes removed since start, by action (expired, noncurrent, marker, aborted).",
		"fs.lifecycle.raced":          "Expired objects left alone since start because they changed before the delete.",
		"fs.lifecycle.failed_buckets": "Buckets the last lifecycle pass could not finish.",
	}

	obs := map[string]metric.Int64ObservableGauge{}
	insts := make([]metric.Observable, 0, len(gauges))

	for name, desc := range gauges {
		g, err := meter.Int64ObservableGauge(name, metric.WithDescription(desc))
		if err != nil {
			return errors.Wrapf(err, "gauge %s", name)
		}

		obs[name] = g
		insts = append(insts, g)
	}

	_, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := sweeper.Stats()

		age := int64(-1)
		if !st.LastPass.IsZero() {
			age = int64(time.Since(st.LastPass).Seconds())
		}

		o.ObserveInt64(obs["fs.lifecycle.age"], age)

		for action, n := range map[string]int{
			"expired":    st.Total.Expired,
			"noncurrent": st.Total.Noncurrent,
			"marker":     st.Total.Markers,
			"aborted":    st.Total.Aborted,
		} {
			o.ObserveInt64(obs["fs.lifecycle.removed"], int64(n), metric.WithAttributes(attribute.String("action", action)))
		}

		o.ObserveInt64(obs["fs.lifecycle.raced"], int64(st.Total.Raced))
		o.ObserveInt64(obs["fs.lifecycle.failed_buckets"], int64(st.Failed))

		return nil
	}, insts...)
	if err != nil {
		return errors.Wrap(err, "register lifecycle metrics")
	}

	return nil
}

// sweepsBucket elects one lifecycle sweeper per bucket in a cluster: the
// first node of the partition the bucket's name maps to that gossip last
// reached. Buckets spread over the nodes, and a node that is down hands its
// buckets to the next. Two nodes that see liveness differently for a moment
// may both sweep a bucket; its deletes are conditional, so the cost is a
// duplicated listing.
func sweepsBucket(member *peer.Member, bucket string) bool {
	l := member.Layout()
	if l == nil {
		return false
	}

	up := map[layout.NodeID]bool{member.ID(): true}

	for _, p := range member.Peers() {
		if p.Err == nil && !p.Seen.IsZero() {
			up[p.ID] = true
		}
	}

	for _, id := range l.Slots[l.Partition([]byte(bucket))] {
		if up[id] {
			return id == member.ID()
		}
	}

	return false
}
