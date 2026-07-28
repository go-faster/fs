// Package memstore is an in-memory, cluster-scope implementation of
// metastore.Store.
//
// It exists for testability and nothing else. Cluster scope is the interesting
// half of the metadata plane — one row per object for the whole cluster, so a
// listing is one scan rather than a merge across nodes — and until the sharded
// pebble plane lands there is otherwise no way to exercise the paths built on
// it. This makes those paths testable without standing up anything.
//
// # What it is not
//
// Not a deployment option, and deliberately not tempting as one: it holds
// everything in memory and loses it all on restart. That is survivable in the
// sense that every metastore.Store is derived — the sidecars are the commit
// point, so a lost store costs a rebuild — but "rebuild the whole cluster's
// metadata on every process start" is not an operational story anyone wants.
//
// The target is the sharded pebble plane. This is scaffolding held to the same
// contract, so that when the real one arrives it implements an interface that
// callers have already been written and tested against.
//
// # Correctness over cleverness
//
// The implementation is a map plus a sort on read. At the sizes a test reaches
// that is fine, and it keeps the code obviously right — which matters, because
// this store's job is to be the thing other tests trust. A subtle bug here
// would look like a bug in whatever is being tested.
package memstore

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/metastore"
)

// Store is an in-memory metadata store.
type Store struct {
	mu      sync.Mutex
	entries map[string]metastore.Entry
	usage   map[string]metastore.Usage
	state   metastore.State
}

var _ metastore.Store = (*Store)(nil)

// New returns an empty store, reporting cluster scope.
//
// It starts in StateBuilding, like every other backend: a store that has not
// been built is not trusted, and the safe default is what makes an unbuilt one
// refuse rather than answer short.
func New() *Store {
	return &Store{
		entries: make(map[string]metastore.Entry),
		usage:   make(map[string]metastore.Usage),
	}
}

// Scope implements metastore.Store.
func (s *Store) Scope() metastore.Scope { return metastore.ScopeCluster }

// Close implements metastore.Store. There is nothing to release.
func (s *Store) Close() error { return nil }

// ref keys an entry. NUL separates because no bucket name may contain one and
// no object key survives XML with one — the same encoding the pebble backend's
// keys use, so the two sort alike.
func ref(bucket, key string) string { return bucket + "\x00" + key }

// Put implements metastore.Store.
func (s *Store) Put(ctx context.Context, e metastore.Entry) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "put index entry")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	prev, found := s.entries[ref(e.Bucket, e.Key)]
	if found && !e.Supersedes(prev) {
		return nil
	}

	// A re-index of the same object keeps whatever the scrub last recorded;
	// only a scrub sets it, and it does not know about writes.
	if found && e.VerifiedAt.IsZero() {
		e.VerifiedAt = prev.VerifiedAt
	}

	delta := metastore.Usage{Objects: 1, Bytes: e.Size}
	if found {
		delta = metastore.Usage{Bytes: e.Size - prev.Size}
	}

	s.entries[ref(e.Bucket, e.Key)] = e
	s.addUsage(e.Bucket, delta)

	return nil
}

// Delete implements metastore.Store. Removing what is not there is not an
// error.
func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "delete index entry")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	prev, found := s.entries[ref(bucket, key)]
	if !found {
		return nil
	}

	delete(s.entries, ref(bucket, key))
	s.addUsage(bucket, metastore.Usage{Objects: -1, Bytes: -prev.Size})

	return nil
}

// addUsage folds a delta into a bucket's counters. The caller holds the lock.
//
// Clamped at zero to match the other backends: counters are derived, and one
// that has gone negative should report nothing rather than nonsense while the
// next rebuild fixes it.
func (s *Store) addUsage(bucket string, delta metastore.Usage) {
	u := s.usage[bucket]
	u.Objects = max(u.Objects+delta.Objects, 0)
	u.Bytes = max(u.Bytes+delta.Bytes, 0)
	s.usage[bucket] = u
}

// Get implements metastore.Store.
func (s *Store) Get(ctx context.Context, bucket, key string) (metastore.Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Entry{}, false, errors.Wrap(err, "read index entry")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	e, found := s.entries[ref(bucket, key)]

	return e, found, nil
}

// Usage implements metastore.Store.
func (s *Store) Usage(ctx context.Context, bucket string) (metastore.Usage, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Usage{}, errors.Wrap(err, "read usage")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.usage[bucket], nil
}

// Buckets implements metastore.Store, in byte order.
func (s *Store) Buckets(ctx context.Context, fn func(bucket string) error) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "scan buckets")
	}

	s.mu.Lock()

	names := make([]string, 0, len(s.usage))
	for name := range s.usage {
		names = append(names, name)
	}

	s.mu.Unlock()

	// Go's string comparison is byte-wise, which is the order S3 specifies and
	// the order every other ordered surface here uses.
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

// Scan implements metastore.Store.
//
// The snapshot is taken under the lock and then walked without it, so a
// callback that itself touches the store cannot deadlock. That costs a copy of
// the matching entries, which at test sizes is nothing and is the right trade
// for a store whose job is to be obviously correct.
func (s *Store) Scan(
	ctx context.Context,
	bucket, prefix, after string,
	limit int,
	fn func(metastore.Entry) error,
) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "scan index")
	}

	s.mu.Lock()

	var matched []metastore.Entry

	for _, e := range s.entries {
		if e.Bucket != bucket || !strings.HasPrefix(e.Key, prefix) || e.Key <= after {
			continue
		}

		matched = append(matched, e)
	}

	s.mu.Unlock()

	slices.SortFunc(matched, func(a, b metastore.Entry) int { return strings.Compare(a.Key, b.Key) })

	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}

	for _, e := range matched {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan index")
		}

		if err := fn(e); err != nil {
			return err
		}
	}

	return nil
}

// SetVerified implements metastore.Store. Objects the store does not hold are
// skipped rather than created: a stamp with no object behind it would be an
// entry that then gets listed.
func (s *Store) SetVerified(ctx context.Context, records []metastore.Verification) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "record verification")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rec := range records {
		e, found := s.entries[ref(rec.Bucket, rec.Key)]
		if !found {
			continue
		}

		e.VerifiedAt = rec.At
		s.entries[ref(rec.Bucket, rec.Key)] = e
	}

	return nil
}

// Coverage implements metastore.Store.
func (s *Store) Coverage(ctx context.Context) (metastore.Coverage, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Coverage{}, errors.Wrap(err, "read coverage")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var cov metastore.Coverage

	for _, e := range s.entries {
		cov.Objects++

		if e.VerifiedAt.IsZero() {
			cov.Never++

			continue
		}

		if cov.Oldest.IsZero() || e.VerifiedAt.Before(cov.Oldest) {
			cov.Oldest = e.VerifiedAt
		}
	}

	return cov, nil
}

// State implements metastore.Store.
func (s *Store) State(ctx context.Context) (metastore.State, error) {
	if err := ctx.Err(); err != nil {
		return metastore.StateBuilding, errors.Wrap(err, "read index state")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state, nil
}

// MarkReady implements metastore.Store.
func (s *Store) MarkReady(ctx context.Context) error {
	return s.setState(ctx, metastore.StateReady)
}

// MarkBuilding implements metastore.Store.
func (s *Store) MarkBuilding(ctx context.Context) error {
	return s.setState(ctx, metastore.StateBuilding)
}

func (s *Store) setState(ctx context.Context, state metastore.State) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "write index state")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = state

	return nil
}

// Reset implements metastore.Store.
func (s *Store) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "reset index")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = make(map[string]metastore.Entry)
	s.usage = make(map[string]metastore.Usage)
	s.state = metastore.StateBuilding

	return nil
}
