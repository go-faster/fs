// Package shardstore is the sharded pebble metadata plane: the target
// implementation of metastore.Store.
//
// This file is routing — turning a key into the node that serves it, and
// keeping that answer current without anyone watching anything.
package shardstore

import (
	"context"
	"sync"

	"github.com/go-faster/errors"
	"golang.org/x/sync/singleflight"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// Loader fetches the current range map.
//
// Injected rather than taking an etcd client, so routing can be tested without
// standing one up and so this package does not depend on the control plane's
// transport. In a node it is a closure over etcd.LoadRangeMap.
type Loader func(ctx context.Context) (*rangemap.Map, error)

// Target is where a key routes, and the map revision it was routed with.
//
// The revision travels with the request. It is what makes a stale route
// correct itself instead of silently reading from the wrong node: the receiver
// compares it against its own map and refuses rather than serving.
type Target struct {
	Range    rangemap.Range
	Revision int64
}

// WrongRange reports that a request arrived at a node that does not own the
// key, carrying the revision that node routed by.
//
// The revision is the useful half. Without it a caller knows only that it was
// wrong; with it, it knows whether refetching the map will help — and a caller
// whose map is already newer is looking at a genuinely unowned key rather than
// a stale cache.
type WrongRange struct {
	// Revision is the responder's map revision.
	Revision int64
	// Key is what was asked for.
	Key string
}

func (e *WrongRange) Error() string {
	return "key " + e.Key + " is not served here at revision " + itoa(e.Revision)
}

// itoa avoids pulling strconv in for one call site in an error path.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}

	var b [20]byte

	i := len(b)

	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}

	return string(b[i:])
}

// Router turns keys into the nodes that serve them.
//
// # Lazy, never watched
//
// The obvious design has every node watch the range map. That is the etcd
// fan-out this whole plane is shaped to avoid: a watch on a 40,000-key prefix
// from ~650 nodes, notified on every split, and splits are routine during
// ingest growth.
//
// So a node routes from its cached map and stamps each request with the
// revision it used. A node that does not own the key rejects with WrongRange
// and its own revision; the caller refetches once and retries. Steady state
// costs nothing at all — no watches, no heartbeats, no map traffic. A change
// costs one wasted round trip per node that routes to the moved range, once.
//
// It is the same trick Ambry uses for failure detection: carry the signal on
// requests that were happening anyway, rather than on a channel that exists to
// carry it.
//
// Because the store is derived and a wrong-owner request is refused rather
// than served, a stale route costs latency and never correctness.
type Router struct {
	load Loader

	// refresh collapses concurrent reloads. Within a node, a map change would
	// otherwise have every in-flight request independently read etcd — which
	// turns one control-plane change into a burst proportional to load, on the
	// component least able to absorb it.
	refresh singleflight.Group

	mu sync.RWMutex
	m  *rangemap.Map
	// want is the highest revision any peer has told us about. The cache is
	// stale while it is behind this.
	want int64
}

// NewRouter returns a router that loads on first use.
func NewRouter(load Loader) *Router { return &Router{load: load} }

// Map returns the cached map, or nil if nothing has been loaded. For admin
// surfaces and metrics; routing goes through Route.
func (r *Router) Map() *rangemap.Map {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.m
}

// Invalidate records that some peer is at a newer revision than we routed by.
//
// Forward only. A peer reporting an *older* revision is itself stale, and
// acting on it would have two nodes refetching each other's staleness in a
// loop that never converges.
func (r *Router) Invalidate(revision int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if revision > r.want {
		r.want = revision
	}
}

// Route returns the target serving a key, loading or refreshing the map when
// the cache is missing or known stale.
func (r *Router) Route(ctx context.Context, key string) (Target, error) {
	m, err := r.current(ctx)
	if err != nil {
		return Target{}, err
	}

	rng, ok := m.Lookup(key)
	if !ok {
		// Not "this key is unowned" — a valid map covers everything. It is a
		// cluster whose metadata plane has not been partitioned yet.
		return Target{}, errors.New("range map is empty: the metadata plane is not initialized")
	}

	return Target{Range: rng, Revision: m.Revision}, nil
}

// Verify is the receiving half: whether this node should serve a key
// according to its own map.
//
// The returned error is a *WrongRange carrying this node's revision, which is
// what lets the caller decide between refetching and giving up.
func (r *Router) Verify(ctx context.Context, key string, self cluster.NodeID) error {
	m, err := r.current(ctx)
	if err != nil {
		return err
	}

	rng, ok := m.Lookup(key)
	if !ok || rng.Owner != self {
		return &WrongRange{Revision: m.Revision, Key: key}
	}

	return nil
}

// Do routes a key, runs fn against the target, and retries once if fn reports
// the route was stale.
//
// Once, deliberately. An unbounded retry against a map that keeps changing is
// a livelock, and one refetch is enough for the case this exists for — a range
// that moved. A second WrongRange after a fresh map means the caller and the
// responder genuinely disagree, which is worth surfacing rather than hiding
// behind another attempt.
func (r *Router) Do(ctx context.Context, key string, fn func(Target) error) error {
	target, err := r.Route(ctx, key)
	if err != nil {
		return err
	}

	err = fn(target)

	var wrong *WrongRange
	if !errors.As(err, &wrong) {
		return err
	}

	r.Invalidate(wrong.Revision)

	target, err = r.Route(ctx, key)
	if err != nil {
		return err
	}

	return fn(target)
}

// current returns the cached map, reloading when it is missing or behind what
// a peer has reported.
func (r *Router) current(ctx context.Context) (*rangemap.Map, error) {
	r.mu.RLock()
	m, want := r.m, r.want
	r.mu.RUnlock()

	if m != nil && m.Revision >= want {
		return m, nil
	}

	loaded, err, _ := r.refresh.Do("map", func() (any, error) {
		// Re-checked inside the flight: a concurrent caller may have already
		// loaded what this one is about to ask for.
		r.mu.RLock()
		m, want := r.m, r.want
		r.mu.RUnlock()

		if m != nil && m.Revision >= want {
			return m, nil
		}

		fresh, err := r.load(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "load range map")
		}

		r.mu.Lock()
		// Only ever move forward. Two flights can overlap around a refresh,
		// and adopting an older map would undo the newer one.
		if r.m == nil || fresh.Revision > r.m.Revision {
			r.m = fresh
		}

		m = r.m
		r.mu.Unlock()

		return m, nil
	})
	if err != nil {
		return nil, err
	}

	fresh, ok := loaded.(*rangemap.Map)
	if !ok || fresh == nil {
		return nil, errors.New("range map loader returned nothing")
	}

	return fresh, nil
}
