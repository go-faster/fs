// Package clusterstore is the replicating storage layer for go-faster/fs
// cluster mode (DESIGN.md §4, ROADMAP.md M3 Phase 7). It composes the pure
// cluster packages — placement (failure-domain-aware HRW), scheme/fragment
// (replication + Reed-Solomon codecs) and transport (authenticated peer API) —
// into the object data plane:
//
//   - Writes ack only after the synchronous write quorum is durable (W=2 full
//     replicas for the replica schemes, all k+m shards for EC) and the
//     remainder — the RF=2.5 parity or the RF=3 trailing replica — is produced
//     behind a bounded async queue. Sub-quorum writes are refused, never
//     silently under-replicated.
//   - Reads consult the object's sidecar (the per-object commit record) and
//     stream the first available replica with open-time failover, or gather
//     any k EC shards, reconstructing missing ones on the fly.
//
// Each write gets a fresh generation stamp: fragments are named by generation
// and the sidecar — replaced atomically per store — is the commit point that
// makes a generation visible, so an overwrite never tears an existing object
// and stale generations are garbage-collected behind the async queue.
// Divergence between targets (e.g. two concurrent overwrites racing on
// different nodes) is reconciled by the repair worker, not the write path.
//
// The fs.Storage implementation on top of this coordinator (bucket metadata,
// listing merge, multipart) is the next Phase 7 slice; see ROADMAP.md.
package clusterstore

import (
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// Sentinel errors of the coordinator.
var (
	// ErrNotFound reports an object with no committed sidecar on any of its
	// placement targets.
	ErrNotFound = errors.New("object not found")
	// ErrInsufficientTargets mirrors fragment.ErrInsufficientTargets: the
	// topology cannot host the scheme's distinct targets, so the write is
	// refused rather than under-protected.
	ErrInsufficientTargets = fragment.ErrInsufficientTargets
	// ErrUnrecoverable mirrors fragment.ErrUnrecoverable: too many fragments
	// are gone to serve the object.
	ErrUnrecoverable = fragment.ErrUnrecoverable
)

// TopologySource provides the current cluster topology snapshot. The etcd
// control plane implements it by caching its watch; tests and single-process
// clusters use StaticTopology.
type TopologySource interface {
	Topology() *cluster.Topology
}

// StaticTopology is a fixed TopologySource for tests and static clusters.
type StaticTopology struct {
	T *cluster.Topology
}

// Topology implements TopologySource.
func (s StaticTopology) Topology() *cluster.Topology { return s.T }

// SchemeFunc resolves the replication scheme for a bucket.
type SchemeFunc func(bucket string) scheme.Scheme

// Config configures a Coordinator.
type Config struct {
	// Topology is the cluster topology source. Required.
	Topology TopologySource
	// Peers dials the peer holding a placement target. Required.
	Peers PeerDialer
	// Scheme resolves the replication scheme per bucket; nil applies
	// scheme.Default (RF=2.5) everywhere.
	Scheme SchemeFunc
	// QueueLen bounds the async remainder queue; when the queue is full the
	// remainder is produced synchronously instead (backpressure, never
	// dropped). Defaults to 128.
	QueueLen int
	// OnAsyncError observes failures of async remainder tasks (the write has
	// already been acknowledged at quorum; the object stays readable and the
	// repair worker will complete it). May be nil.
	OnAsyncError func(bucket, key string, err error)
}

// Coordinator is the cluster object data plane: quorum writes, failover
// reads and deletes of replicated/erasure-coded objects over the peer
// transport. It is safe for concurrent use.
type Coordinator struct {
	topo      TopologySource
	peers     PeerDialer
	schemeFor SchemeFunc
	onErr     func(bucket, key string, err error)

	queue chan func()
	wg    sync.WaitGroup // in-flight async tasks

	mu     sync.Mutex
	closed bool
	worker sync.WaitGroup

	// inflight counts pending async remainder tasks per object, so mutating
	// operations can wait for a key's remainder instead of racing it (e.g. a
	// delete being outrun by the sidecar-extension task, resurrecting the
	// object on the third target).
	inflightMu   sync.Mutex
	inflightCond *sync.Cond
	inflight     map[string]int
}

// New builds a Coordinator and starts its async remainder worker.
func New(cfg Config) (*Coordinator, error) {
	if cfg.Topology == nil {
		return nil, errors.New("clusterstore: Topology is required")
	}

	if cfg.Peers == nil {
		return nil, errors.New("clusterstore: Peers is required")
	}

	schemeFor := cfg.Scheme
	if schemeFor == nil {
		schemeFor = func(string) scheme.Scheme { return scheme.Default }
	}

	queueLen := cfg.QueueLen
	if queueLen <= 0 {
		queueLen = 128
	}

	onErr := cfg.OnAsyncError
	if onErr == nil {
		onErr = func(string, string, error) {}
	}

	c := &Coordinator{
		topo:      cfg.Topology,
		peers:     cfg.Peers,
		schemeFor: schemeFor,
		onErr:     onErr,
		queue:     make(chan func(), queueLen),
		inflight:  make(map[string]int),
	}
	c.inflightCond = sync.NewCond(&c.inflightMu)

	c.worker.Go(func() {
		for task := range c.queue {
			task()
		}
	})

	return c, nil
}

// Topology returns the coordinator's current topology snapshot (for status
// reporting and readiness checks).
func (c *Coordinator) Topology() *cluster.Topology { return c.topo.Topology() }

// objectRef identifies an object across the async machinery.
func objectRef(bucket, key string) string { return bucket + "\x00" + key }

// enqueue schedules an async remainder task for an object, running it inline
// when the queue is full (backpressure) or the coordinator is closed. The
// task is tracked per key until it completes; see waitKey.
func (c *Coordinator) enqueue(bucket, key string, task func()) {
	ref := objectRef(bucket, key)

	c.inflightMu.Lock()
	c.inflight[ref]++
	c.inflightMu.Unlock()

	c.wg.Add(1)

	wrapped := func() {
		defer func() {
			c.inflightMu.Lock()

			c.inflight[ref]--
			if c.inflight[ref] <= 0 {
				delete(c.inflight, ref)
			}

			c.inflightCond.Broadcast()
			c.inflightMu.Unlock()

			c.wg.Done()
		}()

		task()
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		wrapped()

		return
	}

	select {
	case c.queue <- wrapped:
		c.mu.Unlock()
	default:
		c.mu.Unlock()
		wrapped()
	}
}

// waitKey blocks until the object has no pending async remainder work. Every
// mutating operation calls it first: a write, metadata update or delete must
// never race the previous write's remainder (which would, e.g., re-extend a
// deleted object's sidecar onto its third target).
func (c *Coordinator) waitKey(bucket, key string) {
	ref := objectRef(bucket, key)

	c.inflightMu.Lock()
	for c.inflight[ref] > 0 {
		c.inflightCond.Wait()
	}
	c.inflightMu.Unlock()
}

// exclusiveKey acquires the object's async-work slot exclusively: it waits
// for pending remainder work, then holds the slot so mutating operations
// (which waitKey first) block until release. The repairer uses it so a repair
// pass never races a write's remainder — and a write never lands mid-repair
// (where the repairer's stale-generation sweep could delete it).
func (c *Coordinator) exclusiveKey(bucket, key string) (release func()) {
	ref := objectRef(bucket, key)

	c.inflightMu.Lock()
	for c.inflight[ref] > 0 {
		c.inflightCond.Wait()
	}

	c.inflight[ref]++
	c.inflightMu.Unlock()

	return func() {
		c.inflightMu.Lock()

		c.inflight[ref]--
		if c.inflight[ref] <= 0 {
			delete(c.inflight, ref)
		}

		c.inflightCond.Broadcast()
		c.inflightMu.Unlock()
	}
}

// Flush blocks until every async task enqueued so far has completed. It is a
// test and shutdown aid; new writes may enqueue more work concurrently.
func (c *Coordinator) Flush() {
	c.wg.Wait()
}

// Close drains the async queue and stops the worker. The coordinator remains
// usable for reads; subsequent writes produce their remainder synchronously.
func (c *Coordinator) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}

	c.closed = true
	close(c.queue)
	c.mu.Unlock()

	c.worker.Wait()
	c.wg.Wait()

	return nil
}
