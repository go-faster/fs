package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/cluster/peer"
	"github.com/go-faster/fs/internal/sse"
)

// buildEngine opens the engine under root/.engine. member is the cluster
// membership, nil for a single node.
func buildEngine(root string, member *peer.Member, keyring *sse.Keyring, noSync bool) (*engine.Engine, error) {
	if err := refuseLegacyLayout(root); err != nil {
		return nil, err
	}

	return engine.Open(filepath.Join(root, ".engine"), engine.Options{
		Keyring: keyring,
		NoSync:  noSync,
		Member:  member,
	})
}

// registerEngineMetrics exports what an operator needs to see the engine's
// background work keeping up — and the numbers that go wrong first: blocks
// still missing a healthy replica, copies found corrupt, how long ago
// anti-entropy last finished, and deleted rows piling up uncollected.
func registerEngineMetrics(mp metric.MeterProvider, e *engine.Engine) error {
	meter := mp.Meter("github.com/go-faster/fs/engine")

	gauges := map[string]string{
		"fs.engine.blocks.resync_pending": "Block copies waiting for a replica that is missing them.",
		"fs.engine.blocks.corrupt":        "Block copies found not matching their hash since start.",
		"fs.engine.blocks.collected":      "Unreferenced blocks and shards removed since start.",
		"fs.engine.blocks.degraded":       "Reads of erasure-coded blocks rebuilt from parity since start: a shard missing where it should be.",
		"fs.engine.sync.out_of_sync":      "Slots that differed from another replica in the last sweep, by table.",
		"fs.engine.sync.unreachable":      "Replicas that could not be compared in the last sweep, by table.",
		"fs.engine.sync.age":              "Seconds since the last anti-entropy sweep finished, by table.",
		"fs.engine.gc.age":                "Seconds since block collection last finished.",
		"fs.engine.shards.lost":           "Erasure-coded blocks with fewer than K shards: unreadable. Counted by each partition's first node at the last repair sweep.",
		"fs.engine.shards.critical":       "Erasure-coded blocks down to K shards: one more loss from unreadable.",
		"fs.engine.shards.degraded":       "Erasure-coded blocks short of a shard, above K.",
		"fs.engine.shards.unreachable":    "Partitions the last shard repair sweep could not see whole.",
		"fs.engine.shards.rebuilt":        "Shards rebuilt here from the others since start.",
		"fs.engine.shards.handed_over":    "Shards moved to the node the layout gives them since start.",
		"fs.engine.shards.age":            "Seconds since the last shard repair sweep finished.",
		"fs.engine.tombstones.queued":     "Deleted rows waiting to be collected at the last pass, by table.",
		"fs.engine.tombstones.collected":  "Deleted rows removed from every replica since start, by table.",
		"fs.engine.tombstones.deferred":   "Deleted rows left for a later pass, a replica unreachable, since start, by table.",
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

	age := func(t time.Time) int64 {
		if t.IsZero() {
			return -1 // Never: distinct from "just now".
		}

		return int64(time.Since(t).Seconds())
	}

	_, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := e.Stats()

		o.ObserveInt64(obs["fs.engine.blocks.resync_pending"], s.Blocks.ResyncPending)
		o.ObserveInt64(obs["fs.engine.blocks.corrupt"], s.Blocks.Corrupt)
		o.ObserveInt64(obs["fs.engine.blocks.collected"], s.Blocks.Collected)
		o.ObserveInt64(obs["fs.engine.blocks.degraded"], s.Blocks.Degraded)
		o.ObserveInt64(obs["fs.engine.gc.age"], age(s.LastGC))
		o.ObserveInt64(obs["fs.engine.shards.lost"], int64(s.Shards.Lost))
		o.ObserveInt64(obs["fs.engine.shards.critical"], int64(s.Shards.Critical))
		o.ObserveInt64(obs["fs.engine.shards.degraded"], int64(s.Shards.Degraded))
		o.ObserveInt64(obs["fs.engine.shards.unreachable"], int64(s.Shards.Unreachable))
		o.ObserveInt64(obs["fs.engine.shards.rebuilt"], s.Shards.Rebuilt)
		o.ObserveInt64(obs["fs.engine.shards.handed_over"], s.Shards.HandedOver)
		o.ObserveInt64(obs["fs.engine.shards.age"], age(s.Shards.LastSweep))

		sweeps := map[string]struct {
			last             time.Time
			out, unreachable int
		}{"blocks": {s.BlockSync.LastSweep, s.BlockSync.OutOfSync, s.BlockSync.Unreachable}}

		for name, t := range s.TableSync {
			sweeps[name] = struct {
				last             time.Time
				out, unreachable int
			}{t.LastSweep, t.OutOfSync, t.Unreachable}
		}

		for name, gc := range s.Tombstones {
			attr := metric.WithAttributes(attribute.String("table", name))
			o.ObserveInt64(obs["fs.engine.tombstones.queued"], int64(gc.Queued), attr)
			o.ObserveInt64(obs["fs.engine.tombstones.collected"], gc.Collected, attr)
			o.ObserveInt64(obs["fs.engine.tombstones.deferred"], gc.Deferred, attr)
		}

		for name, sw := range sweeps {
			attr := metric.WithAttributes(attribute.String("table", name))
			o.ObserveInt64(obs["fs.engine.sync.out_of_sync"], int64(sw.out), attr)
			o.ObserveInt64(obs["fs.engine.sync.unreachable"], int64(sw.unreachable), attr)
			o.ObserveInt64(obs["fs.engine.sync.age"], age(sw.last), attr)
		}

		return nil
	}, insts...)
	if err != nil {
		return errors.Wrap(err, "register engine metrics")
	}

	return nil
}

// refuseLegacyLayout stops the server on a data directory the removed
// filesystem backend wrote. The engine would start on it as an empty store,
// and the objects would look lost rather than unread.
func refuseLegacyLayout(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return errors.Wrap(err, "read storage root")
	}

	for _, e := range entries {
		name := e.Name()

		legacy := false

		switch name {
		case ".tmp", ".meta", ".multipart", ".versions", ".quarantine":
			legacy = true
		default:
			legacy = e.IsDir() && !strings.HasPrefix(name, ".")
		}

		if legacy {
			return errors.Errorf(
				"%s holds data written by the filesystem backend (found %q), which this release no longer reads; "+
					"copy the objects out with the previous release and into a new storage root", root, name)
		}
	}

	return nil
}
