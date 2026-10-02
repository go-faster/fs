package main

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"time"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
	"github.com/go-faster/fs/internal/cluster/table"
	"github.com/go-faster/fs/internal/engine"
	"github.com/go-faster/fs/internal/sse"
)

// StorageTypeEngine is the storage engine over replicated metadata tables and
// content-addressed blocks: a single node, or a cluster when cluster.node_id
// is set.
const StorageTypeEngine = "engine"

// soloID is a single node's identity in its own one-node layout.
const soloID = "local"

// buildEngine opens the engine's metadata database and block store under
// root. member is the cluster membership, or nil for a single node, which
// gets a private one with a one-node layout.
func buildEngine(root string, member *peer.Member, keyring *sse.Keyring) (*engine.Engine, error) {
	dir := filepath.Join(root, ".engine")

	if member == nil {
		var err error
		if member, err = soloMember(filepath.Join(dir, "solo")); err != nil {
			return nil, err
		}
	}

	db, err := table.OpenDB(filepath.Join(dir, "meta.db"))
	if err != nil {
		return nil, err
	}

	store, err := block.NewStore(filepath.Join(dir, "blocks"))
	if err != nil {
		return nil, err
	}

	return engine.New(engine.Config{
		Member:  member,
		DB:      db,
		Blocks:  block.NewManager(store, member),
		Keyring: keyring,
	})
}

// soloMember is a single node's membership: never served, with a one-node
// layout applied the first time.
func soloMember(dir string) (*peer.Member, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, errors.Wrap(err, "solo secret")
	}

	m, err := peer.New(peer.Config{ID: soloID, Addr: soloID, Secret: secret, Dir: dir})
	if err != nil {
		return nil, errors.Wrap(err, "solo membership")
	}

	if m.Layout() == nil {
		roles := []layout.Node{{ID: soloID, Capacity: 1}}
		if _, _, err := m.Apply(roles, layout.Options{Partitions: 1, Widths: []int{1}}, false); err != nil {
			return nil, errors.Wrap(err, "solo layout")
		}
	}

	return m, nil
}

// registerEngineMetrics exports what an operator needs to see the engine's
// background work keeping up — and the numbers that go wrong first: blocks
// still missing a healthy replica, copies found corrupt, and how long ago
// anti-entropy last finished.
func registerEngineMetrics(mp metric.MeterProvider, e *engine.Engine) error {
	meter := mp.Meter("github.com/go-faster/fs/engine")

	gauges := map[string]string{
		"fs.engine.blocks.resync_pending": "Block copies waiting for a replica that is missing them.",
		"fs.engine.blocks.corrupt":        "Block copies found not matching their hash since start.",
		"fs.engine.blocks.collected":      "Unreferenced blocks removed since start.",
		"fs.engine.sync.out_of_sync":      "Slots that differed from another replica in the last sweep, by table.",
		"fs.engine.sync.unreachable":      "Replicas that could not be compared in the last sweep, by table.",
		"fs.engine.sync.age":              "Seconds since the last anti-entropy sweep finished, by table.",
		"fs.engine.gc.age":                "Seconds since block collection last finished.",
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
		o.ObserveInt64(obs["fs.engine.gc.age"], age(s.LastGC))

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
