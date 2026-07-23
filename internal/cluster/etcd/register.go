package etcd

import (
	"context"
	"sync"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/cluster"
)

// reRegisterBackoff is the pause before re-acquiring a lost lease.
const reRegisterBackoff = time.Second

// Registration keeps a node present in the cluster registry: a lease-bound
// key refreshed by keepalives, re-acquired with backoff if the lease is ever
// lost (etcd restart, partition longer than the TTL). Close removes the node
// from the registry.
type Registration struct {
	cancel context.CancelFunc
	done   sync.WaitGroup

	key string

	mu      sync.Mutex
	client  *clientv3.Client
	leaseID clientv3.LeaseID
	// value is the current registry payload; Update replaces it (disk usage
	// refresh) and lease re-acquisition re-publishes the latest.
	value string
}

// Register announces a node in the registry and keeps it alive until Close.
// It returns once the node is durably registered (first lease + put
// complete); subsequent lease losses are recovered in the background.
func Register(ctx context.Context, client *clientv3.Client, cfg Config, node cluster.Node) (*Registration, error) {
	cfg = cfg.withDefaults()

	value, err := encodeNode(node)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &Registration{cancel: cancel, client: client, value: string(value), key: cfg.nodeKey(node.ID)}
	key := r.key

	keepalive, err := r.acquire(ctx, runCtx, cfg, key)
	if err != nil {
		cancel()
		return nil, err
	}

	r.done.Go(func() { r.run(runCtx, cfg, key, keepalive) })

	return r, nil
}

// Update replaces the node's registry payload in place (same lease) — used to
// refresh per-disk capacity without touching membership. Lease re-acquisition
// after a loss republishes the latest value.
func (r *Registration) Update(ctx context.Context, node cluster.Node) error {
	value, err := encodeNode(node)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.value = string(value)
	leaseID := r.leaseID
	key := r.key
	r.mu.Unlock()

	if _, err := r.client.Put(ctx, key, string(value), clientv3.WithLease(leaseID)); err != nil {
		// The background loop re-publishes the value when it re-acquires the
		// lease; an update racing a lease loss is not fatal.
		return errors.Wrap(err, "update registry key")
	}

	return nil
}

// acquire grants a lease, writes the registry key under it and starts the
// keepalive stream. The synchronous setup calls honor ctx (the caller's
// deadline on Register); the keepalive stream binds to streamCtx — the
// registration's lifetime — so Close reliably ends it (a stream on the
// caller's ctx would outlive Close and deadlock the drain loop).
func (r *Registration) acquire(ctx, streamCtx context.Context, cfg Config, key string) (<-chan *clientv3.LeaseKeepAliveResponse, error) {
	lease, err := r.client.Grant(ctx, cfg.TTL)
	if err != nil {
		return nil, errors.Wrap(err, "grant registration lease")
	}

	r.mu.Lock()
	value := r.value
	r.mu.Unlock()

	if _, err := r.client.Put(ctx, key, value, clientv3.WithLease(lease.ID)); err != nil {
		return nil, errors.Wrap(err, "write registry key")
	}

	keepalive, err := r.client.KeepAlive(streamCtx, lease.ID)
	if err != nil {
		return nil, errors.Wrap(err, "start lease keepalive")
	}

	r.mu.Lock()
	r.leaseID = lease.ID
	r.mu.Unlock()

	return keepalive, nil
}

// run consumes keepalives and re-acquires the lease whenever the stream ends,
// until Close cancels the context.
func (r *Registration) run(ctx context.Context, cfg Config, key string, keepalive <-chan *clientv3.LeaseKeepAliveResponse) {
	for {
		if keepalive != nil {
			for range keepalive { //nolint:revive // Draining; responses carry nothing actionable.
			}
		}

		// Keepalive stream ended (or the last re-acquire failed): shutdown,
		// or the lease was lost.
		if contextDone(ctx) {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(reRegisterBackoff):
		}

		next, err := r.acquire(ctx, ctx, cfg, key)
		if err != nil {
			// Land back on the backoff next round.
			keepalive = nil
			continue
		}

		keepalive = next
	}
}

// Close stops the keepalive loop and revokes the lease, removing the node
// from the registry immediately (rather than after the TTL).
func (r *Registration) Close() error {
	r.cancel()
	r.done.Wait()

	r.mu.Lock()
	leaseID := r.leaseID
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := r.client.Revoke(ctx, leaseID); err != nil {
		return errors.Wrap(err, "revoke registration lease")
	}

	return nil
}
