package etcd

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/cluster"
)

// rewatchBackoff is the pause before re-establishing a broken watch.
const rewatchBackoff = time.Second

// Source watches the node registry and maintains an epoch-stamped
// cluster.Topology snapshot. It implements clusterstore.TopologySource:
// Topology is a lock-free read of the latest snapshot, safe on every data
// path. The epoch is the etcd revision the snapshot reflects, so placement is
// stable within an epoch.
type Source struct {
	client *clientv3.Client
	cfg    Config

	cur atomic.Pointer[cluster.Topology]

	// mu guards nodes, overrides and the publish that reads them. Two watches
	// feed this — the registry and the weight overrides — so the maps are no
	// longer touched by a single goroutine.
	mu    sync.Mutex
	nodes map[cluster.NodeID]cluster.Node

	// overrides is the per-disk weight each node's registration is merged
	// with, keyed by node then disk (fs SPEC §11.6).
	overrides map[cluster.NodeID]map[cluster.DiskID]float64

	cancel context.CancelFunc
	done   sync.WaitGroup

	// OnError observes background watch failures (the source keeps serving
	// the last snapshot and retries). Set before any topology change happens;
	// may be nil.
	OnError func(err error)
}

// NewSource loads the current registry and starts watching it. The returned
// source serves a valid topology immediately.
func NewSource(ctx context.Context, client *clientv3.Client, cfg Config) (*Source, error) {
	cfg = cfg.withDefaults()

	s := &Source{
		client:    client,
		cfg:       cfg,
		nodes:     make(map[cluster.NodeID]cluster.Node),
		overrides: make(map[cluster.NodeID]map[cluster.DiskID]float64),
	}

	// Overrides load first: a snapshot published before they are known would
	// briefly place onto a disk an operator has drained.
	overrideRev, err := s.loadOverrides(ctx)
	if err != nil {
		return nil, err
	}

	rev, err := s.load(ctx)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.done.Go(func() { s.watch(runCtx, rev+1) })
	s.done.Go(func() { s.watchOverrides(runCtx, overrideRev+1) })

	return s, nil
}

// Topology implements clusterstore.TopologySource.
func (s *Source) Topology() *cluster.Topology { return s.cur.Load() }

// Close stops the watch. Topology keeps serving the last snapshot.
func (s *Source) Close() error {
	s.cancel()
	s.done.Wait()

	return nil
}

// load fetches the full registry and publishes the initial snapshot,
// returning the revision it reflects.
func (s *Source) load(ctx context.Context) (int64, error) {
	resp, err := s.client.Get(ctx, s.cfg.nodesPrefix(), clientv3.WithPrefix())
	if err != nil {
		return 0, errors.Wrap(err, "load node registry")
	}

	s.mu.Lock()

	for _, kv := range resp.Kvs {
		node, err := decodeNode(kv.Value)
		if err != nil {
			// A malformed record must not take the control plane down; the
			// node is simply absent until it re-registers cleanly.
			continue
		}

		s.nodes[node.ID] = node
	}

	s.mu.Unlock()

	s.publish(uint64(resp.Header.Revision)) //nolint:gosec // etcd revisions are non-negative.

	return resp.Header.Revision, nil
}

// watch applies registry events from rev onward, re-establishing the watch
// (with a fresh full load) whenever it breaks.
func (s *Source) watch(ctx context.Context, rev int64) {
	for {
		ch := s.client.Watch(ctx, s.cfg.nodesPrefix(), clientv3.WithPrefix(), clientv3.WithRev(rev))

		for resp := range ch {
			if err := resp.Err(); err != nil {
				s.reportErr(errors.Wrap(err, "registry watch"))
				break
			}

			for _, ev := range resp.Events {
				s.apply(ev)
			}

			rev = resp.Header.Revision + 1

			s.publish(uint64(resp.Header.Revision)) //nolint:gosec // etcd revisions are non-negative.
		}

		if contextDone(ctx) {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(rewatchBackoff):
		}

		// The watch broke (compaction, leader loss): resync from a full load
		// so no event is missed, then watch from the loaded revision.
		s.mu.Lock()
		clear(s.nodes)
		s.mu.Unlock()

		loaded, err := s.load(ctx)
		if err != nil {
			s.reportErr(err)
			continue
		}

		rev = loaded + 1
	}
}

// apply folds one registry event into the node map.
func (s *Source) apply(ev *clientv3.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch ev.Type {
	case clientv3.EventTypeDelete:
		id := cluster.NodeID(ev.Kv.Key[len(s.cfg.nodesPrefix()):])
		delete(s.nodes, id)
	default:
		node, err := decodeNode(ev.Kv.Value)
		if err != nil {
			s.reportErr(err)
			return
		}

		s.nodes[node.ID] = node
	}
}

// publish snapshots the node map as the current topology. Nodes are sorted by
// ID so snapshots are deterministic.
func (s *Source) publish(epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nodes := make([]cluster.Node, 0, len(s.nodes))

	for _, n := range s.nodes {
		// The node says which disks exist and what they hold; the override
		// says what placement should do with them. Merging here means every
		// consumer — placement, the rebalancer, the status view, the
		// topology Signature — sees one effective weight and none of them
		// has to know an override exists (fs SPEC §11.6).
		if weights := s.overrides[n.ID]; len(weights) > 0 {
			disks := make([]cluster.Disk, len(n.Disks))
			copy(disks, n.Disks)

			for i := range disks {
				if weight, ok := weights[disks[i].ID]; ok {
					disks[i].Weight = weight
				}
			}

			n.Disks = disks
		}

		nodes = append(nodes, n)
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	s.cur.Store(&cluster.Topology{Epoch: epoch, Nodes: nodes})
}

// loadOverrides fetches every disk weight override, returning the revision it
// reflects.
func (s *Source) loadOverrides(ctx context.Context) (int64, error) {
	resp, err := s.client.Get(ctx, s.cfg.overridesPrefix(), clientv3.WithPrefix())
	if err != nil {
		return 0, errors.Wrap(err, "load disk weight overrides")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, kv := range resp.Kvs {
		s.applyOverride(string(kv.Key), kv.Value, false)
	}

	return resp.Header.Revision, nil
}

// watchOverrides applies override events, resyncing on a broken watch the
// same way the registry does.
func (s *Source) watchOverrides(ctx context.Context, rev int64) {
	for {
		ch := s.client.Watch(ctx, s.cfg.overridesPrefix(), clientv3.WithPrefix(), clientv3.WithRev(rev))

		for resp := range ch {
			if err := resp.Err(); err != nil {
				s.reportErr(errors.Wrap(err, "disk weight watch"))
				break
			}

			s.mu.Lock()

			for _, ev := range resp.Events {
				s.applyOverride(string(ev.Kv.Key), ev.Kv.Value, ev.Type == clientv3.EventTypeDelete)
			}

			s.mu.Unlock()

			rev = resp.Header.Revision + 1

			s.publish(uint64(resp.Header.Revision)) //nolint:gosec // etcd revisions are non-negative.
		}

		if contextDone(ctx) {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(rewatchBackoff):
		}

		s.mu.Lock()
		clear(s.overrides)
		s.mu.Unlock()

		loaded, err := s.loadOverrides(ctx)
		if err != nil {
			s.reportErr(err)
			continue
		}

		// Republish: the resync may have changed what is drained.
		s.publish(uint64(loaded)) //nolint:gosec // etcd revisions are non-negative.

		rev = loaded + 1
	}
}

// applyOverride folds one override event into the map. The caller holds mu.
func (s *Source) applyOverride(key string, value []byte, deleted bool) {
	node, disk, ok := splitDiskWeightKey(s.cfg, key)
	if !ok {
		return
	}

	if deleted {
		if weights := s.overrides[node]; weights != nil {
			delete(weights, disk)

			if len(weights) == 0 {
				delete(s.overrides, node)
			}
		}

		return
	}

	override, ok := decodeDiskWeight(s.cfg, key, value)
	if !ok {
		return
	}

	if s.overrides[node] == nil {
		s.overrides[node] = make(map[cluster.DiskID]float64)
	}

	s.overrides[node][disk] = override.Weight
}

// reportErr forwards a background error to the hook, if set.
func (s *Source) reportErr(err error) {
	if s.OnError != nil {
		s.OnError(err)
	}
}
