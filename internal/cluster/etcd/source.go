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

	cur    atomic.Pointer[cluster.Topology]
	nodes  map[cluster.NodeID]cluster.Node
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
		client: client,
		cfg:    cfg,
		nodes:  make(map[cluster.NodeID]cluster.Node),
	}

	rev, err := s.load(ctx)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.done.Go(func() { s.watch(runCtx, rev+1) })

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

	for _, kv := range resp.Kvs {
		node, err := decodeNode(kv.Value)
		if err != nil {
			// A malformed record must not take the control plane down; the
			// node is simply absent until it re-registers cleanly.
			continue
		}

		s.nodes[node.ID] = node
	}

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
		clear(s.nodes)

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
	nodes := make([]cluster.Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		nodes = append(nodes, n)
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	s.cur.Store(&cluster.Topology{Epoch: epoch, Nodes: nodes})
}

// reportErr forwards a background error to the hook, if set.
func (s *Source) reportErr(err error) {
	if s.OnError != nil {
		s.OnError(err)
	}
}
