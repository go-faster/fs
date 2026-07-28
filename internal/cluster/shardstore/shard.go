package shardstore

import (
	"bytes"
	"context"
	"encoding/json"
	"hash/fnv"
	"path/filepath"
	"sync"

	"github.com/cockroachdb/pebble/v2"
	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/keyspace"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// ErrNotOwned reports that a key falls outside every range this shard serves.
//
// It is a sentinel rather than a *WrongRange because a shard does not know the
// map revision — it knows only what it was told to serve. The layer that routed
// the request has the revision and turns this into the answer a caller can act
// on. Keeping the two apart is what stops a shard having to hold a view of the
// cluster in order to refuse one key.
var ErrNotOwned = errors.New("key is not in a range served by this shard")

// stripes is how many locks guard the read-modify-write behind an update. A
// bucket's counters move with the entry that displaces them, so updates to one
// bucket serialize; different buckets do not contend.
const stripes = 64

// Shard is this node's half of the sharded metadata plane: one pebble instance
// holding the ranges this node serves, as key intervals inside it.
//
// # One instance, ranges as intervals
//
// Not one instance per range. N instances on a node means N memtables and N
// write-ahead logs fsyncing to one device, which turns a sequential write
// pattern into a random one and multiplies memory by N. Ranges are intervals in
// a single instance, which also makes every operation a primitive pebble
// already has — a bounded iterator to serve one, Ingest to adopt one, Excise to
// drop one.
//
// # Counters are partial on purpose
//
// A bucket's objects span ranges, so a bucket's counters cannot live in one
// place without every write becoming a cross-shard write — two nodes, no atomic
// batch, and counters that drift from the rows behind them.
//
// So each shard counts only what it holds. The entry and the counter it moves
// are written in one pebble batch, exactly as the node-local store does, so a
// shard's counters can never disagree with its own rows; and a bucket's real
// total is the sum across shards. That is the same shape local scope already
// has — per-node counters summed across nodes — and it means no range becomes
// the hot spot that holding every bucket's counters would create.
type Shard struct {
	db   *pebble.DB
	dir  string
	once sync.Once
	// write is how durably an entry lands. Unsynced by default for the same
	// reason the node-local store is: the plane is derived, so a crash costs a
	// rebuild rather than an object.
	write *pebble.WriteOptions

	// ship sends what this shard applies to its ranges' followers. Nil is the
	// unreplicated configuration.
	ship Shipper

	mu sync.RWMutex
	// owned is what this shard serves; followed is what it replicates for
	// another node and deliberately does not serve.
	owned    []rangemap.Range
	followed []rangemap.Range

	locks [stripes]sync.Mutex
}

// ShardOption configures a Shard.
type ShardOption func(*Shard)

// WithShardSyncWrites makes every entry durable before the call returns.
//
// Off by default. The trade is the one the node-local store already makes: an
// unsynced shard is rebuilt after an unclean stop, and paying an fsync per
// entry buys nothing a rebuild does not already guarantee.
func WithShardSyncWrites(durable bool) ShardOption {
	return func(s *Shard) {
		if durable {
			s.write = pebble.Sync

			return
		}

		s.write = pebble.NoSync
	}
}

// OpenShard opens (or creates) this node's shard under dir.
//
// It serves nothing until Adopt names the ranges it owns. That is deliberate:
// a shard that guessed would serve keys the map has given to someone else.
func OpenShard(dir string, opts ...ShardOption) (*Shard, error) {
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, errors.Wrapf(err, "open shard at %q", dir)
	}

	s := &Shard{db: db, dir: dir, write: pebble.NoSync}

	for _, o := range opts {
		o(s)
	}

	return s, nil
}

// DefaultShardDir is where a node keeps its shard, given the storage root.
func DefaultShardDir(root string) string { return filepath.Join(root, "cluster", "shard") }

// Dir is where the shard lives.
func (s *Shard) Dir() string { return s.dir }

// Close releases the shard.
func (s *Shard) Close() error {
	var err error

	s.once.Do(func() {
		if cerr := s.db.Close(); cerr != nil {
			err = errors.Wrap(cerr, "close shard")
		}
	})

	return err
}

// Flush makes everything written so far visible to the size estimates.
//
// Needed because those read table metadata, and data still in the memtable is
// data pebble cannot account for. In a running node this is not called at all —
// the estimates are advisory and a range that looks smaller than it is simply
// splits a little later — but a test that writes and immediately measures is
// otherwise measuring nothing.
func (s *Shard) Flush() error {
	if err := s.db.Flush(); err != nil {
		return errors.Wrap(err, "flush shard")
	}

	return nil
}

// Adopt sets the ranges this shard serves.
//
// Called after every map change. It replaces rather than merges, because a
// range this node no longer owns must stop being served immediately — the new
// owner is already serving it, and two nodes answering for one key is how a
// listing returns a superseded record.
//
// Adopting does not move data. What a shard holds outside its ranges is stale
// and unreachable, and dropping it is a separate operation with its own cost.
func (s *Shard) Adopt(ranges []rangemap.Range) {
	owned := make([]rangemap.Range, len(ranges))
	copy(owned, ranges)

	s.mu.Lock()
	s.owned = owned
	s.mu.Unlock()
}

// Ranges returns what this shard serves, in key order.
func (s *Shard) Ranges() []rangemap.Range {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]rangemap.Range, len(s.owned))
	copy(out, s.owned)

	return out
}

// owns reports whether an encoded key falls in a served range.
func (s *Shard) owns(key []byte) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, r := range s.owned {
		if r.Contains(string(key)) {
			return true
		}
	}

	return false
}

// lock returns the stripe guarding a bucket's updates.
func (s *Shard) lock(bucket string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucket))

	return &s.locks[h.Sum32()%stripes]
}

// Put records an object, adjusting this shard's counters by what it displaces.
//
// A record that does not supersede the stored one is dropped, by the same total
// order the sidecars use — an older sidecar can arrive after a newer one, and
// letting it win would resurrect a stale size in both the entry and the
// counters.
func (s *Shard) Put(ctx context.Context, e metastore.Entry) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "put index entry")
	}

	if !s.owns(keyspace.ObjectKey(e.Bucket, e.Key)) {
		return ErrNotOwned
	}

	return s.store(e, s.write, func(key, repr []byte) {
		// After the commit, never before: a replica must not be told about a
		// write the owner has not made, or a failover could promote one holding
		// something the disks never had.
		s.shipTo(ctx, key, repr)
	})
}

// store is Put without the ownership check: the supersede rules, the counter
// delta and the one batch that keeps them together.
//
// Separated so a backfill can use it. A learner is not the owner and Put would
// refuse it, but everything after that check is exactly what a backfilled entry
// needs — and reimplementing it would be reimplementing the ordering rule,
// which is the one piece of this store that must not have a second copy.
//
// after runs with the committed batch, before it is closed. Only an owner
// passes one: a learner storing a backfilled entry ships nothing, because it is
// not the node anyone replicates from.
func (s *Shard) store(e metastore.Entry, write *pebble.WriteOptions, after func(key, repr []byte)) error {
	key := keyspace.ObjectKey(e.Bucket, e.Key)

	l := s.lock(e.Bucket)
	l.Lock()
	defer l.Unlock()

	prev, found, err := s.get(e.Bucket, e.Key)
	if err != nil {
		return err
	}

	if found && !e.Supersedes(prev) {
		return nil
	}

	// A re-index keeps whatever the scrub last recorded; only a scrub sets it,
	// and it does not know about writes.
	if found && e.VerifiedAt.IsZero() {
		e.VerifiedAt = prev.VerifiedAt
	}

	delta := metastore.Usage{Objects: 1, Bytes: e.Size}
	if found {
		delta = metastore.Usage{Bytes: e.Size - prev.Size}
	}

	data, err := json.Marshal(e)
	if err != nil {
		return errors.Wrap(err, "marshal index entry")
	}

	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()

	if err := batch.Set(key, data, nil); err != nil {
		return errors.Wrap(err, "stage index entry")
	}

	if err := s.stageUsage(batch, e.Bucket, delta); err != nil {
		return err
	}

	// One batch, so an entry and the counter it moves can never disagree —
	// which is what makes this shard's share of a bucket exact without a
	// recount.
	if err := batch.Commit(write); err != nil {
		return errors.Wrap(err, "commit index entry")
	}

	if after != nil {
		after(key, batch.Repr())
	}

	return nil
}

// Delete removes an object and takes its bytes back out of the counters.
// Removing what is not there is not an error.
func (s *Shard) Delete(ctx context.Context, bucket, key string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "delete index entry")
	}

	encoded := keyspace.ObjectKey(bucket, key)
	if !s.owns(encoded) {
		return ErrNotOwned
	}

	l := s.lock(bucket)
	l.Lock()
	defer l.Unlock()

	prev, found, err := s.get(bucket, key)
	if err != nil {
		return err
	}

	if !found {
		return nil
	}

	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()

	if err := batch.Delete(encoded, nil); err != nil {
		return errors.Wrap(err, "stage index delete")
	}

	if err := s.stageUsage(batch, bucket, metastore.Usage{Objects: -1, Bytes: -prev.Size}); err != nil {
		return err
	}

	if err := batch.Commit(s.write); err != nil {
		return errors.Wrap(err, "commit index delete")
	}

	s.shipTo(ctx, encoded, batch.Repr())

	return nil
}

// stageUsage folds a delta into this shard's counters within batch. The caller
// holds the bucket's stripe, so the read cannot race another update.
func (s *Shard) stageUsage(batch *pebble.Batch, bucket string, delta metastore.Usage) error {
	usage, err := s.usage(bucket)
	if err != nil {
		return err
	}

	usage.Objects = max(usage.Objects+delta.Objects, 0)
	usage.Bytes = max(usage.Bytes+delta.Bytes, 0)

	data, err := json.Marshal(usage)
	if err != nil {
		return errors.Wrap(err, "marshal usage")
	}

	if err := batch.Set(keyspace.UsageKey(bucket), data, nil); err != nil {
		return errors.Wrap(err, "stage usage")
	}

	return nil
}

// Get returns one object's entry, if this shard serves it.
func (s *Shard) Get(ctx context.Context, bucket, key string) (metastore.Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Entry{}, false, errors.Wrap(err, "read index entry")
	}

	if !s.owns(keyspace.ObjectKey(bucket, key)) {
		return metastore.Entry{}, false, ErrNotOwned
	}

	return s.get(bucket, key)
}

func (s *Shard) get(bucket, key string) (metastore.Entry, bool, error) {
	value, closer, err := s.db.Get(keyspace.ObjectKey(bucket, key))
	if errors.Is(err, pebble.ErrNotFound) {
		return metastore.Entry{}, false, nil
	}

	if err != nil {
		return metastore.Entry{}, false, errors.Wrap(err, "read index entry")
	}

	defer func() { _ = closer.Close() }()

	var e metastore.Entry
	if err := json.Unmarshal(value, &e); err != nil {
		return metastore.Entry{}, false, errors.Wrap(err, "unmarshal index entry")
	}

	return e, true, nil
}

// Usage returns this shard's share of a bucket's counters.
//
// Partial by design — a bucket's objects span ranges, and the total is the sum
// across shards. See the type doc.
func (s *Shard) Usage(ctx context.Context, bucket string) (metastore.Usage, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Usage{}, errors.Wrap(err, "read usage")
	}

	return s.usage(bucket)
}

func (s *Shard) usage(bucket string) (metastore.Usage, error) {
	value, closer, err := s.db.Get(keyspace.UsageKey(bucket))
	if errors.Is(err, pebble.ErrNotFound) {
		return metastore.Usage{}, nil
	}

	if err != nil {
		return metastore.Usage{}, errors.Wrap(err, "read usage")
	}

	defer func() { _ = closer.Close() }()

	var u metastore.Usage
	if err := json.Unmarshal(value, &u); err != nil {
		return metastore.Usage{}, errors.Wrap(err, "unmarshal usage")
	}

	return u, nil
}

// Buckets calls fn for every bucket this shard holds counters for, in name
// order.
func (s *Shard) Buckets(ctx context.Context, fn func(bucket string) error) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "scan buckets")
	}

	lower := []byte{keyspace.Usage}

	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: keyspace.UpperBound(lower),
	})
	if err != nil {
		return errors.Wrap(err, "open shard iterator")
	}

	defer func() { _ = iter.Close() }()

	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan buckets")
		}

		// Copied: iter.Key() is only valid until the next Next, and a caller
		// collecting names retains them.
		if err := fn(string(iter.Key()[1:])); err != nil {
			return err
		}
	}

	if err := iter.Error(); err != nil {
		return errors.Wrap(err, "scan buckets")
	}

	return nil
}

// Scan calls fn for each of a bucket's objects this shard serves, in key order.
//
// Bounded to the served ranges, and to their intersection with the request —
// so a shard owning part of a bucket returns exactly its part, in order, and
// the layer above concatenates. It never returns a key it does not serve, and
// never silently skips one it does.
func (s *Shard) Scan(
	ctx context.Context,
	bucket, prefix, after string,
	limit int,
	fn func(metastore.Entry) error,
) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "scan index")
	}

	lower := keyspace.BucketPrefix(bucket)
	if prefix != "" {
		lower = append(lower, prefix...)
	}

	upper := keyspace.UpperBound(lower)

	// Start past the cursor when one is given and it falls inside the range.
	start := lower
	if after != "" {
		if candidate := append(keyspace.BucketPrefix(bucket), after...); bytes.Compare(candidate, lower) >= 0 {
			start = append(candidate, 0)
		}
	}

	count := 0

	// The served ranges are disjoint and in key order, so walking them in turn
	// yields the shard's share of the bucket in key order too.
	for _, r := range s.Ranges() {
		done, err := s.scanRange(ctx, r, start, upper, limit, &count, fn)
		if err != nil || done {
			return err
		}
	}

	return nil
}

// ScanRange serves one range's share of a bucket, in key order.
//
// The Store drives listings this way rather than calling Scan once per owner,
// and the reason is ordering. A node can own range 1 and range 3 while another
// owns range 2; asking each owner for its whole share would return keys from 1
// and 3 together, and concatenating that with 2 puts the page out of order. A
// k-way merge would fix it and cost a comparison per key. Walking the ranges in
// order and asking each owner for exactly one of them costs nothing and cannot
// be got wrong.
//
// The range must be one this shard serves; anything else is ErrNotOwned, so a
// stale map cannot quietly produce a page with a hole in it.
func (s *Shard) ScanRange(
	ctx context.Context,
	r rangemap.Range,
	bucket, prefix, after string,
	limit int,
	fn func(metastore.Entry) error,
) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "scan index")
	}

	if !s.serves(r) {
		return ErrNotOwned
	}

	lower := keyspace.BucketPrefix(bucket)
	if prefix != "" {
		lower = append(lower, prefix...)
	}

	upper := keyspace.UpperBound(lower)

	start := lower
	if after != "" {
		if candidate := append(keyspace.BucketPrefix(bucket), after...); bytes.Compare(candidate, lower) >= 0 {
			start = append(candidate, 0)
		}
	}

	count := 0

	_, err := s.scanRange(ctx, r, start, upper, limit, &count, fn)

	return err
}

// serves reports whether this shard was told to serve exactly this range.
func (s *Shard) serves(r rangemap.Range) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, owned := range s.owned {
		if owned.Start == r.Start && owned.End == r.End {
			return true
		}
	}

	return false
}

// scanRange serves the part of [start, upper) that falls inside r.
func (s *Shard) scanRange(
	ctx context.Context,
	r rangemap.Range,
	start, upper []byte,
	limit int,
	count *int,
	fn func(metastore.Entry) error,
) (done bool, err error) {
	from := start
	if []byte(r.Start) != nil && bytes.Compare([]byte(r.Start), from) > 0 {
		from = []byte(r.Start)
	}

	to := upper
	if r.End != "" && (to == nil || bytes.Compare([]byte(r.End), to) < 0) {
		to = []byte(r.End)
	}

	if to != nil && bytes.Compare(from, to) >= 0 {
		// This range does not overlap what was asked for.
		return false, nil
	}

	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: from, UpperBound: to})
	if err != nil {
		return false, errors.Wrap(err, "open shard iterator")
	}

	defer func() { _ = iter.Close() }()

	for iter.First(); iter.Valid(); iter.Next() {
		if limit > 0 && *count >= limit {
			return true, nil
		}

		if err := ctx.Err(); err != nil {
			return false, errors.Wrap(err, "scan index")
		}

		var e metastore.Entry
		if err := json.Unmarshal(iter.Value(), &e); err != nil {
			// One unreadable entry must not end a listing; a rebuild fixes it.
			continue
		}

		if err := fn(e); err != nil {
			return false, err
		}

		*count++
	}

	if err := iter.Error(); err != nil {
		return false, errors.Wrap(err, "scan index")
	}

	return false, nil
}

// SetVerified records when the scrub last checked these objects. Objects this
// shard does not hold are skipped rather than created.
func (s *Shard) SetVerified(ctx context.Context, records []metastore.Verification) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "record verification")
	}

	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()

	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "record verification")
		}

		if !s.owns(keyspace.ObjectKey(rec.Bucket, rec.Key)) {
			continue
		}

		l := s.lock(rec.Bucket)
		l.Lock()

		entry, found, err := s.get(rec.Bucket, rec.Key)
		if err != nil {
			l.Unlock()

			return err
		}

		if !found {
			l.Unlock()

			continue
		}

		entry.VerifiedAt = rec.At

		data, err := json.Marshal(entry)
		if err != nil {
			l.Unlock()

			return errors.Wrap(err, "marshal index entry")
		}

		err = batch.Set(keyspace.ObjectKey(rec.Bucket, rec.Key), data, nil)

		l.Unlock()

		if err != nil {
			return errors.Wrap(err, "stage verification")
		}
	}

	if batch.Empty() {
		return nil
	}

	if err := batch.Commit(s.write); err != nil {
		return errors.Wrap(err, "commit verification")
	}

	return nil
}

// Coverage reports how stale verification is across what this shard holds.
// Partial, like Usage: the cluster's coverage is the combination across shards.
func (s *Shard) Coverage(ctx context.Context) (metastore.Coverage, error) {
	var cov metastore.Coverage

	buckets := make([]string, 0, 8)

	if err := s.Buckets(ctx, func(bucket string) error {
		buckets = append(buckets, bucket)

		return nil
	}); err != nil {
		return metastore.Coverage{}, err
	}

	for _, bucket := range buckets {
		err := s.Scan(ctx, bucket, "", "", 0, func(e metastore.Entry) error {
			cov.Objects++

			if e.VerifiedAt.IsZero() {
				cov.Never++

				return nil
			}

			if cov.Oldest.IsZero() || e.VerifiedAt.Before(cov.Oldest) {
				cov.Oldest = e.VerifiedAt
			}

			return nil
		})
		if err != nil {
			return metastore.Coverage{}, err
		}
	}

	return cov, nil
}

// Reset empties the shard, for a rebuild that must not inherit stale entries —
// an object deleted while nothing was watching leaves one behind, and nothing
// in a rebuild would otherwise remove it.
func (s *Shard) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "reset shard")
	}

	for _, prefix := range [][]byte{{keyspace.Object}, {keyspace.Usage}} {
		if err := s.db.DeleteRange(prefix, keyspace.UpperBound(prefix), pebble.Sync); err != nil {
			return errors.Wrap(err, "reset shard")
		}
	}

	return nil
}

// DropUnowned removes the entries outside the served ranges, then rebuilds this
// shard's counters from what is left.
//
// Separate from Adopt because it is not free: a range handed to another node
// stops being served the moment the map says so, but reclaiming its space is
// bulk work, and doing it inside a map change would make every ownership change
// as slow as the data it moves.
//
// # Only the object prefix is swept, and the counters are recomputed
//
// A shard's usage rows are keyed 'u' + bucket, which sorts above every object
// key — so a sweep bounded at the end of the *usage* prefix deletes them, and a
// shard that narrowed its ranges would report zero usage for buckets it still
// serves. That is what this used to do.
//
// Sweeping only the object prefix fixes that half and leaves the other: the
// counters still describe entries that have just been dropped. They cannot be
// adjusted incrementally, because what was removed is exactly what is no longer
// there to be counted. So they are rebuilt from the remaining entries — a scan
// of what the shard holds, which is the same order of work as the drop it
// follows, and this is already the bulk path.
func (s *Shard) DropUnowned(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "drop unowned ranges")
	}

	// The gaps between served ranges, walked in key order.
	var from []byte

	for _, r := range s.Ranges() {
		if r.Start != "" && (from == nil || bytes.Compare([]byte(r.Start), from) > 0) {
			if err := s.dropBetween(from, []byte(r.Start)); err != nil {
				return err
			}
		}

		if r.End == "" {
			from = nil

			break
		}

		from = []byte(r.End)
	}

	if from != nil || len(s.Ranges()) == 0 {
		if err := s.dropBetween(from, nil); err != nil {
			return err
		}
	}

	return s.recount(ctx)
}

// dropBetween deletes the object entries in [from, to), where nil bounds mean
// the ends of the object prefix.
func (s *Shard) dropBetween(from, to []byte) error {
	objects := []byte{keyspace.Object}
	objectsEnd := keyspace.UpperBound(objects)

	lower := from
	if lower == nil || bytes.Compare(lower, objects) < 0 {
		lower = objects
	}

	upper := to
	if upper == nil || bytes.Compare(upper, objectsEnd) > 0 {
		upper = objectsEnd
	}

	if bytes.Compare(lower, upper) >= 0 {
		return nil
	}

	if err := s.db.DeleteRange(lower, upper, pebble.Sync); err != nil {
		return errors.Wrap(err, "drop unowned range")
	}

	return nil
}

// recount rebuilds every usage row from the entries this shard still holds.
//
// Rows for buckets it no longer holds anything of are removed rather than
// zeroed, so Buckets does not keep reporting a bucket this shard has nothing
// in — which would make the cluster-wide bucket list include names no shard
// can produce an object for.
func (s *Shard) recount(ctx context.Context) error {
	totals := make(map[string]metastore.Usage)

	lower := []byte{keyspace.Object}

	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: keyspace.UpperBound(lower),
	})
	if err != nil {
		return errors.Wrap(err, "open shard iterator")
	}

	defer func() { _ = iter.Close() }()

	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "recount usage")
		}

		var e metastore.Entry
		if err := json.Unmarshal(iter.Value(), &e); err != nil {
			continue
		}

		u := totals[e.Bucket]
		u.Objects++
		u.Bytes += e.Size
		totals[e.Bucket] = u
	}

	if err := iter.Error(); err != nil {
		return errors.Wrap(err, "recount usage")
	}

	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()

	// Every existing row is cleared and only the recomputed ones written back,
	// so a bucket that lost its last entry here leaves no row behind.
	usage := []byte{keyspace.Usage}
	if err := batch.DeleteRange(usage, keyspace.UpperBound(usage), nil); err != nil {
		return errors.Wrap(err, "clear usage")
	}

	for bucket, u := range totals {
		data, err := json.Marshal(u)
		if err != nil {
			return errors.Wrap(err, "marshal usage")
		}

		if err := batch.Set(keyspace.UsageKey(bucket), data, nil); err != nil {
			return errors.Wrap(err, "stage usage")
		}
	}

	if err := batch.Commit(pebble.Sync); err != nil {
		return errors.Wrap(err, "commit usage")
	}

	return nil
}
