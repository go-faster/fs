package shardstore

import (
	"context"
	"slices"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/keyspace"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// Backend is what one shard can answer for.
//
// Satisfied by *Shard for the node's own ranges, and by a peer client for
// everyone else's. The Store does not care which it is holding, which is what
// keeps the fan-out logic — the part that is easy to get subtly wrong — free of
// any notion of local versus remote.
type Backend interface {
	Put(ctx context.Context, e metastore.Entry) error
	Get(ctx context.Context, bucket, key string) (metastore.Entry, bool, error)
	Delete(ctx context.Context, bucket, key string) error
	Usage(ctx context.Context, bucket string) (metastore.Usage, error)
	Buckets(ctx context.Context, fn func(bucket string) error) error
	ScanRange(ctx context.Context, r rangemap.Range, bucket, prefix, after string,
		limit int, fn func(metastore.Entry) error) error
	SetVerified(ctx context.Context, records []metastore.Verification) error
	Coverage(ctx context.Context) (metastore.Coverage, error)
	Reset(ctx context.Context) error
	// Measure reports a range's size and where it would divide, asked of the
	// node that owns it — the only one holding it.
	Measure(ctx context.Context, r rangemap.Range) (Measurement, error)
}

var _ Backend = (*Shard)(nil)

// Resolver returns the backend serving a node.
type Resolver func(ctx context.Context, node cluster.NodeID) (Backend, error)

// Readiness is the cluster-wide ready/building flag.
//
// Cluster-scope rather than per node, because the plane is usable only when all
// of it is. A per-node flag would let a node that finished its own share report
// ready while the cluster is half rebuilt, and a listing served then reports a
// fraction of the cluster as all of it — the one outcome worse than refusing to
// answer.
type Readiness interface {
	State(ctx context.Context) (metastore.State, error)
	Set(ctx context.Context, state metastore.State) error
}

// Store is the sharded pebble metadata plane as one metastore.Store.
//
// It holds no data. Every operation is routed to the shards that serve the keys
// involved, and what this type contributes is the composition: which shards to
// ask, in what order, and how to combine what they say.
//
// That composition is the whole of the interesting work here, and it differs
// per operation:
//
//   - A keyed operation goes to exactly one shard.
//   - A listing walks the ranges the bucket spans, **in key order**, asking each
//     range's owner for that range alone.
//   - A bucket's usage is the sum of its shards' partial counters.
//   - Coverage is the combination of partial coverages.
//   - Readiness is neither: it is one cluster-wide flag.
type Store struct {
	router  *Router
	resolve Resolver
	ready   Readiness
}

var _ metastore.Store = (*Store)(nil)

// NewStore builds the cluster-scope store.
func NewStore(router *Router, resolve Resolver, ready Readiness) *Store {
	return &Store{router: router, resolve: resolve, ready: ready}
}

// Scope implements metastore.Store: one row per object for the whole cluster.
func (s *Store) Scope() metastore.Scope { return metastore.ScopeCluster }

// Close implements metastore.Store. The shards are owned by whoever opened
// them; this type holds none.
func (s *Store) Close() error { return nil }

// backend resolves the shard serving a routed target, turning a shard's
// ownership refusal into one the caller can act on.
func (s *Store) backend(ctx context.Context, target Target) (Backend, error) {
	return s.resolve(ctx, target.Range.Owner)
}

// onKey routes a single key and runs fn against its shard, retrying once if the
// route turns out to have been stale.
//
// The ErrNotOwned translation is the point. A shard refuses with a sentinel
// because it does not know the map revision; here we do, so it becomes a
// WrongRange the router can act on — which is what turns a stale route into one
// wasted round trip instead of a wrong answer.
func (s *Store) onKey(ctx context.Context, bucket, key string, fn func(Backend) error) error {
	encoded := string(keyspace.ObjectKey(bucket, key))

	return s.router.Do(ctx, encoded, func(target Target) error {
		b, err := s.backend(ctx, target)
		if err != nil {
			return err
		}

		if err := fn(b); errors.Is(err, ErrNotOwned) {
			return &WrongRange{Revision: target.Revision, Key: encoded}
		} else if err != nil {
			return err
		}

		return nil
	})
}

// Put implements metastore.Store.
func (s *Store) Put(ctx context.Context, e metastore.Entry) error {
	return s.onKey(ctx, e.Bucket, e.Key, func(b Backend) error { return b.Put(ctx, e) })
}

// Delete implements metastore.Store.
func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	return s.onKey(ctx, bucket, key, func(b Backend) error { return b.Delete(ctx, bucket, key) })
}

// Get implements metastore.Store.
func (s *Store) Get(ctx context.Context, bucket, key string) (metastore.Entry, bool, error) {
	var (
		entry metastore.Entry
		found bool
	)

	err := s.onKey(ctx, bucket, key, func(b Backend) error {
		var err error

		entry, found, err = b.Get(ctx, bucket, key)

		return err
	})

	return entry, found, err
}

// bucketRanges returns the ranges a bucket's keys span, in key order.
//
// A bucket occupies one contiguous interval of the key space, so this is the
// slice of ranges overlapping it — usually one, and more only for a bucket
// large enough to have been split.
func (s *Store) bucketRanges(ctx context.Context, bucket, prefix string) ([]rangemap.Range, error) {
	lower := keyspace.BucketPrefix(bucket)
	if prefix != "" {
		lower = append(lower, prefix...)
	}

	upper := keyspace.UpperBound(lower)

	// Routing the lower bound both loads the map and gives the revision, so
	// the caller sees a consistent partition for the whole page.
	if _, err := s.router.Route(ctx, string(lower)); err != nil {
		return nil, err
	}

	m := s.router.Map()
	if m == nil {
		return nil, errors.New("range map is empty: the metadata plane is not initialized")
	}

	var out []rangemap.Range

	for _, r := range m.Ranges {
		if upper != nil && r.Start >= string(upper) {
			break
		}

		if r.End != "" && r.End <= string(lower) {
			continue
		}

		out = append(out, r)
	}

	return out, nil
}

// Scan implements metastore.Store, walking the ranges a bucket spans in key
// order and asking each range's owner for that range alone.
//
// One query per range the page intersects, which is one for almost every page:
// a range holds millions of entries, so a page crosses a boundary only when it
// happens to start near one. That is the property the whole plane exists for —
// a cost that does not grow with the cluster — and it is why the walk is by
// range rather than by owner. Asking each owner for its whole share would be
// fewer calls and the wrong order, since one node can own ranges either side of
// another's.
func (s *Store) Scan(
	ctx context.Context,
	bucket, prefix, after string,
	limit int,
	fn func(metastore.Entry) error,
) error {
	ranges, err := s.bucketRanges(ctx, bucket, prefix)
	if err != nil {
		return err
	}

	count := 0
	cursor := after

	for _, r := range ranges {
		if limit > 0 && count >= limit {
			return nil
		}

		remaining := 0
		if limit > 0 {
			remaining = limit - count
		}

		b, err := s.resolve(ctx, r.Owner)
		if err != nil {
			return err
		}

		err = b.ScanRange(ctx, r, bucket, prefix, cursor, remaining, func(e metastore.Entry) error {
			count++
			cursor = e.Key

			return fn(e)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// owners returns the distinct nodes serving a bucket, and the whole cluster's
// nodes when bucket is empty.
func (s *Store) owners(ctx context.Context, bucket string) ([]cluster.NodeID, error) {
	var ranges []rangemap.Range

	if bucket == "" {
		if _, err := s.router.Route(ctx, ""); err != nil {
			return nil, err
		}

		m := s.router.Map()
		if m == nil {
			return nil, errors.New("range map is empty: the metadata plane is not initialized")
		}

		ranges = m.Ranges
	} else {
		var err error
		if ranges, err = s.bucketRanges(ctx, bucket, ""); err != nil {
			return nil, err
		}
	}

	var out []cluster.NodeID

	for _, r := range ranges {
		if !slices.Contains(out, r.Owner) {
			out = append(out, r.Owner)
		}
	}

	return out, nil
}

// Usage implements metastore.Store by summing the shards' partial counters.
//
// Asked once per distinct owner rather than once per range: a shard's counters
// are per bucket, not per range, so an owner holding three of a bucket's ranges
// reports its share once and asking again would double it.
func (s *Store) Usage(ctx context.Context, bucket string) (metastore.Usage, error) {
	owners, err := s.owners(ctx, bucket)
	if err != nil {
		return metastore.Usage{}, err
	}

	var total metastore.Usage

	for _, node := range owners {
		b, err := s.resolve(ctx, node)
		if err != nil {
			return metastore.Usage{}, err
		}

		part, err := b.Usage(ctx, bucket)
		if err != nil {
			return metastore.Usage{}, err
		}

		total.Objects += part.Objects
		total.Bytes += part.Bytes
	}

	return total, nil
}

// Buckets implements metastore.Store as the union across shards, in name order.
//
// Collected rather than merged. Each shard answers in order, so a k-way merge
// would stream — but the names also have to be deduplicated, because a bucket
// spanning shards is reported by each of them, and a streaming dedup across N
// sorted inputs is a heap for a result bounded by bucket count rather than
// object count. This is the place to revisit if bucket-per-tenant deployments
// grow past what fits comfortably in memory; it is not the place that decides
// whether the plane scales.
func (s *Store) Buckets(ctx context.Context, fn func(bucket string) error) error {
	owners, err := s.owners(ctx, "")
	if err != nil {
		return err
	}

	seen := make(map[string]struct{})

	for _, node := range owners {
		b, err := s.resolve(ctx, node)
		if err != nil {
			return err
		}

		if err := b.Buckets(ctx, func(bucket string) error {
			seen[bucket] = struct{}{}

			return nil
		}); err != nil {
			return err
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan buckets")
		}

		if err := fn(name); err != nil {
			return err
		}
	}

	return nil
}

// SetVerified implements metastore.Store, grouping stamps by the shard that
// holds them so each is asked once.
func (s *Store) SetVerified(ctx context.Context, records []metastore.Verification) error {
	if len(records) == 0 {
		return nil
	}

	grouped := make(map[cluster.NodeID][]metastore.Verification)

	for _, rec := range records {
		target, err := s.router.Route(ctx, string(keyspace.ObjectKey(rec.Bucket, rec.Key)))
		if err != nil {
			return err
		}

		grouped[target.Range.Owner] = append(grouped[target.Range.Owner], rec)
	}

	for node, batch := range grouped {
		b, err := s.resolve(ctx, node)
		if err != nil {
			return err
		}

		if err := b.SetVerified(ctx, batch); err != nil {
			return err
		}
	}

	return nil
}

// Coverage implements metastore.Store by combining the shards' partials.
//
// Counts add; the oldest verification is the least recent across all of them,
// which is the honest answer — coverage is only as good as the least recently
// checked object anywhere, not as good as the best shard.
func (s *Store) Coverage(ctx context.Context) (metastore.Coverage, error) {
	owners, err := s.owners(ctx, "")
	if err != nil {
		return metastore.Coverage{}, err
	}

	var total metastore.Coverage

	for _, node := range owners {
		b, err := s.resolve(ctx, node)
		if err != nil {
			return metastore.Coverage{}, err
		}

		part, err := b.Coverage(ctx)
		if err != nil {
			return metastore.Coverage{}, err
		}

		total.Objects += part.Objects
		total.Never += part.Never

		if !part.Oldest.IsZero() && (total.Oldest.IsZero() || part.Oldest.Before(total.Oldest)) {
			total.Oldest = part.Oldest
		}
	}

	return total, nil
}

// State implements metastore.Store.
func (s *Store) State(ctx context.Context) (metastore.State, error) {
	return s.ready.State(ctx)
}

// MarkReady implements metastore.Store.
func (s *Store) MarkReady(ctx context.Context) error {
	return s.ready.Set(ctx, metastore.StateReady)
}

// MarkBuilding implements metastore.Store.
func (s *Store) MarkBuilding(ctx context.Context) error {
	return s.ready.Set(ctx, metastore.StateBuilding)
}

// Reset implements metastore.Store, emptying every shard.
//
// Marked building first. A reset that emptied the shards before saying so would
// leave a window where the plane reads ready and holds nothing, and a listing
// served in it reports an empty cluster as the truth.
func (s *Store) Reset(ctx context.Context) error {
	if err := s.MarkBuilding(ctx); err != nil {
		return err
	}

	owners, err := s.owners(ctx, "")
	if err != nil {
		return err
	}

	for _, node := range owners {
		b, err := s.resolve(ctx, node)
		if err != nil {
			return err
		}

		if err := b.Reset(ctx); err != nil {
			return err
		}
	}

	return nil
}

// MemoryReadiness is an in-memory cluster-wide flag, usable from its zero
// value and starting at StateBuilding — a plane that has not been built is not
// trusted.
//
// For tests and for a single-node plane. A real deployment keeps this in etcd,
// which is what makes every node see the same answer; an in-memory one on each
// node would have each of them believing its own share is the cluster.
type MemoryReadiness struct {
	mu    sync.Mutex
	state metastore.State
}

// State implements Readiness.
func (m *MemoryReadiness) State(context.Context) (metastore.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.state, nil
}

// Set implements Readiness.
func (m *MemoryReadiness) Set(_ context.Context, state metastore.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.state = state

	return nil
}
