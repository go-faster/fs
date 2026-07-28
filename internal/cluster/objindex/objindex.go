// Package objindex is the pebble-backed, node-local implementation of
// metastore.Store.
//
// It exists because the only cluster-wide record of an object is its sidecar,
// so every question about the object set — list this bucket, how many objects
// does it hold, which have not been verified lately — currently means reading
// every sidecar on every disk. That is the walk behind listings, the usage
// recount and the scrub sweep alike, and at the design target of 100M objects
// per node it does not finish.
//
// The index is **derived, never authoritative**. Sidecars remain the commit
// point: they are self-describing and sit next to the data, so a disk stays
// interpretable on its own and repair needs no external state. Losing this
// index costs a rebuild from those same disks and nothing else, which is why
// it can be kept fast and unsynced — see Open.
//
// Its scope is one node: it describes what this node's disks hold, so a
// cluster-wide answer is a merge across nodes. See metastore.Scope.
package objindex

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
)

// stateKey holds whether the index was handed over cleanly by the last
// process.
var stateKey = []byte{keyspace.Meta, 's'}

// versionKey holds the entry format the stored entries were written with.
var versionKey = []byte{keyspace.Meta, 'v'}

// entryVersion stamps the shape of a stored entry. Bumping it makes the next
// start discard what it finds and rebuild from the disks, which is what a
// changed shape needs and what being pre-production makes affordable.
//
// 3 added metastore.Entry.Locator. The field is absent from every stored entry
// written at 2 and would decode as zero, which is the correct value — so the
// bump is not strictly required to read them. It is taken anyway, because the
// mechanism exists to make exactly this decision unnecessary to think about,
// and an upgrade that rebuilds is cheaper than one that is subtly wrong.
const entryVersion = 3

// stripes is how many locks guard the read-modify-write behind an update. A
// bucket's counters have to be adjusted with the entry that moves them, so
// updates to one bucket serialize; different buckets do not contend.
const stripes = 64

// Index is a node's object index.
type Index struct {
	db   *pebble.DB
	mu   [stripes]sync.Mutex
	dir  string
	once sync.Once
	// write is how durably an entry lands. It is one field on purpose: it is
	// the whole difference between an index that is rebuilt after a crash and
	// one that is never behind an acknowledged write. See WithSyncWrites.
	write *pebble.WriteOptions
	// adopted is what the previous process left behind: whether it handed the
	// index over cleanly, at a shape this binary can read. See Adopted.
	adopted bool
}

var _ metastore.Store = (*Index)(nil)

// Option configures an Index.
type Option func(*Index)

// WithSyncWrites makes every entry durable before the call returns.
//
// Off (the default) the index is rebuilt after an unclean stop, because a crash
// can swallow writes the disks already took — the rebuild is what makes that
// safe rather than silent. On, an acknowledged object write is never missing
// from the index, at the cost of a write-ahead-log fsync per object on top of
// the fragment's own.
//
// This is the knob to reach for before considering an index that is
// authoritative rather than derived: it removes the staleness window without
// moving the commit point off the sidecars, which are what keep a disk
// interpretable on its own.
func WithSyncWrites(durable bool) Option {
	return func(i *Index) {
		if durable {
			i.write = pebble.Sync

			return
		}

		i.write = pebble.NoSync
	}
}

// Open opens (or creates) the index under dir.
//
// Writes are not synced. That is deliberate and safe only because of what
// happens next: Open immediately records the index as building, and only Close
// records it ready. A process that dies takes the marker with it, so the next
// start finds "building" and rebuilds from the disks rather than trusting
// counters that may be short by whatever the crash swallowed. Paying for an
// fsync per object write would buy nothing a rebuild does not already
// guarantee.
func Open(dir string, opts ...Option) (*Index, error) {
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, errors.Wrapf(err, "open index at %q", dir)
	}

	idx := &Index{db: db, dir: dir, write: pebble.NoSync}

	for _, o := range opts {
		o(idx)
	}

	// What the last process left behind. Read before anything overwrites it,
	// because writing the answer down is the only way it survives Open —
	// see Adopted.
	state, err := idx.state()
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	// Entries written in another shape cannot be read as this one, so they are
	// not adopted: the stamp is corrected and the index rebuilt.
	stored, err := idx.entryVersion()
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	if stored != entryVersion {
		if err := idx.setEntryVersion(entryVersion); err != nil {
			_ = db.Close()

			return nil, err
		}

		state = metastore.StateBuilding
	}

	idx.adopted = state == metastore.StateReady

	// Invalidate before serving a single write: from here on only an orderly
	// Close can call the index ready.
	//
	// Unconditional, and that is the fix rather than the simplification. This
	// used to skip the write when the index did not already read ready — which
	// the version-mismatch branch had just arranged, in a local variable, on
	// its way to concluding that a rebuild was owed. The persisted state
	// therefore stayed *ready*, and a node upgraded across a format change
	// adopted a stale-shaped index instead of rebuilding it: the version stamp
	// made a rebuild less likely, not more.
	//
	// It had never mattered because the stamp had never been bumped. Bumping it
	// for the locator field is what surfaced it.
	if err := idx.setState(metastore.StateBuilding); err != nil {
		_ = db.Close()

		return nil, err
	}

	return idx, nil
}

// Scope implements metastore.Store: this index describes one node's holdings,
// so a cluster-wide answer is a merge across nodes.
func (i *Index) Scope() metastore.Scope { return metastore.ScopeLocal }

// Close implements metastore.Store. It records the index as ready and closes
// it; an index that is not closed is not damaged — it is rebuilt.
func (i *Index) Close() error {
	var err error

	i.once.Do(func() {
		if serr := i.setState(metastore.StateReady); serr != nil {
			err = serr
		}

		if cerr := i.db.Close(); cerr != nil && err == nil {
			err = errors.Wrap(cerr, "close index")
		}
	})

	return err
}

// Dir is where the index lives. It is not part of metastore.Store: a store
// backed by anything other than a local database has no directory to name.
func (i *Index) Dir() string { return i.dir }

// Adopted reports whether the previous process handed this index over cleanly,
// at a shape this binary can read — that is, whether its contents can be
// trusted without a rebuild.
//
// It exists because Open destroys the evidence. Open must record the index as
// building before serving a single write, so that a process which then dies
// leaves the next start something to rebuild from; but that overwrite is also
// the only record of how the *last* process ended. Reading State after Open
// therefore always says "building" and cannot distinguish a clean shutdown from
// a crash, which is how every restart came to re-walk every disk — the exact
// scan the index exists to avoid.
//
// So the answer is captured at Open and read from here. It is fixed for the
// lifetime of the index: it describes one handover, not the current state.
//
// A caller that adopts must mark the index ready itself. Open deliberately
// leaves it building, so an adopting caller that forgets would leave the node
// excluded from listings for as long as it runs.
func (i *Index) Adopted() bool { return i.adopted }

// State implements metastore.Store.
func (i *Index) State(ctx context.Context) (metastore.State, error) {
	if err := ctx.Err(); err != nil {
		return metastore.StateBuilding, errors.Wrap(err, "read index state")
	}

	return i.state()
}

func (i *Index) state() (metastore.State, error) {
	value, closer, err := i.db.Get(stateKey)
	if errors.Is(err, pebble.ErrNotFound) {
		return metastore.StateBuilding, nil
	}

	if err != nil {
		return metastore.StateBuilding, errors.Wrap(err, "read index state")
	}

	defer func() { _ = closer.Close() }()

	if len(value) == 1 && value[0] == byte(metastore.StateReady) {
		return metastore.StateReady, nil
	}

	return metastore.StateBuilding, nil
}

// MarkReady implements metastore.Store. Close records readiness too; this is
// for a build that completes while the process keeps running.
func (i *Index) MarkReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "write index state")
	}

	return i.setState(metastore.StateReady)
}

// MarkBuilding implements metastore.Store.
func (i *Index) MarkBuilding(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "write index state")
	}

	return i.setState(metastore.StateBuilding)
}

// entryVersion reports the format of the stored entries; a missing marker
// reads as version zero, which never matches and so rebuilds.
func (i *Index) entryVersion() (int, error) {
	value, closer, err := i.db.Get(versionKey)
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, nil
	}

	if err != nil {
		return 0, errors.Wrap(err, "read index version")
	}

	defer func() { _ = closer.Close() }()

	if len(value) != 1 {
		return 0, nil
	}

	return int(value[0]), nil
}

func (i *Index) setEntryVersion(v int) error {
	if v < 0 || v > 255 {
		return errors.Errorf("index version %d does not fit a byte", v)
	}

	if err := i.db.Set(versionKey, []byte{byte(v)}, pebble.Sync); err != nil {
		return errors.Wrap(err, "write index version")
	}

	return nil
}

func (i *Index) setState(s metastore.State) error {
	if err := i.db.Set(stateKey, []byte{byte(s)}, pebble.Sync); err != nil {
		return errors.Wrap(err, "write index state")
	}

	return nil
}

// lock returns the stripe guarding a bucket's updates.
func (i *Index) lock(bucket string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucket))

	return &i.mu[h.Sum32()%stripes]
}

// Put implements metastore.Store, adjusting the bucket's counters by what the
// record displaces.
//
// A record that does not supersede the stored one is dropped: an older sidecar
// can arrive after a newer one — a rebalance copying a superseded generation,
// a repair completing late — and letting it win would resurrect a stale size
// in both the entry and the counters.
func (i *Index) Put(ctx context.Context, e metastore.Entry) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "put index entry")
	}

	l := i.lock(e.Bucket)
	l.Lock()
	defer l.Unlock()

	prev, found, err := i.get(e.Bucket, e.Key)
	if err != nil {
		return err
	}

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

	data, err := json.Marshal(e)
	if err != nil {
		return errors.Wrap(err, "marshal index entry")
	}

	batch := i.db.NewBatch()
	defer func() { _ = batch.Close() }()

	if err := batch.Set(keyspace.ObjectKey(e.Bucket, e.Key), data, nil); err != nil {
		return errors.Wrap(err, "stage index entry")
	}

	if err := i.stageUsage(batch, e.Bucket, delta); err != nil {
		return err
	}

	// One batch, so an entry and the counters it moves can never disagree —
	// which is what makes usage exact per node without a recount.
	if err := batch.Commit(i.write); err != nil {
		return errors.Wrap(err, "commit index entry")
	}

	return nil
}

// Delete implements metastore.Store. Removing what is not there is not an
// error.
func (i *Index) Delete(ctx context.Context, bucket, key string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "delete index entry")
	}

	l := i.lock(bucket)
	l.Lock()
	defer l.Unlock()

	prev, found, err := i.get(bucket, key)
	if err != nil {
		return err
	}

	if !found {
		return nil
	}

	batch := i.db.NewBatch()
	defer func() { _ = batch.Close() }()

	if err := batch.Delete(keyspace.ObjectKey(bucket, key), nil); err != nil {
		return errors.Wrap(err, "stage index delete")
	}

	if err := i.stageUsage(batch, bucket, metastore.Usage{Objects: -1, Bytes: -prev.Size}); err != nil {
		return err
	}

	if err := batch.Commit(i.write); err != nil {
		return errors.Wrap(err, "commit index delete")
	}

	return nil
}

// stageUsage folds a delta into the bucket's counters within batch. The caller
// holds the bucket's stripe, so the read cannot race another update.
func (i *Index) stageUsage(batch *pebble.Batch, bucket string, delta metastore.Usage) error {
	usage, err := i.usage(bucket)
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

// Get implements metastore.Store.
func (i *Index) Get(ctx context.Context, bucket, key string) (metastore.Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Entry{}, false, errors.Wrap(err, "read index entry")
	}

	return i.get(bucket, key)
}

func (i *Index) get(bucket, key string) (metastore.Entry, bool, error) {
	value, closer, err := i.db.Get(keyspace.ObjectKey(bucket, key))
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

// Usage implements metastore.Store, returning a bucket's counters as this node
// holds them.
func (i *Index) Usage(ctx context.Context, bucket string) (metastore.Usage, error) {
	if err := ctx.Err(); err != nil {
		return metastore.Usage{}, errors.Wrap(err, "read usage")
	}

	return i.usage(bucket)
}

func (i *Index) usage(bucket string) (metastore.Usage, error) {
	value, closer, err := i.db.Get(keyspace.UsageKey(bucket))
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

// Scan implements metastore.Store.
//
// This is the shape a listing needs and the reason for the whole package: the
// answer costs what the page contains rather than what the bucket holds.
//
// Cancellation is checked once per entry rather than once per call. A scan with
// no limit walks the bucket, so a caller that gave up — a listing whose client
// disconnected, a coverage pass whose node is shutting down — would otherwise
// keep a node reading to the end of a disk on nobody's behalf.
func (i *Index) Scan(
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

	// Start past the boundary when one is given and it is inside the range.
	start := lower
	if after != "" {
		if candidate := append(keyspace.BucketPrefix(bucket), after...); bytes.Compare(candidate, lower) >= 0 {
			start = append(candidate, 0)
		}
	}

	iter, err := i.db.NewIter(&pebble.IterOptions{
		LowerBound: start,
		UpperBound: keyspace.UpperBound(lower),
	})
	if err != nil {
		return errors.Wrap(err, "open index iterator")
	}

	defer func() { _ = iter.Close() }()

	count := 0

	for iter.First(); iter.Valid(); iter.Next() {
		if limit > 0 && count >= limit {
			return nil
		}

		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan index")
		}

		var e metastore.Entry
		if err := json.Unmarshal(iter.Value(), &e); err != nil {
			// One unreadable entry must not end a listing; a rebuild fixes it.
			continue
		}

		if err := fn(e); err != nil {
			return err
		}

		count++
	}

	if err := iter.Error(); err != nil {
		return errors.Wrap(err, "scan index")
	}

	return nil
}

// Buckets implements metastore.Store, in key order because the usage prefix is
// stored that way.
func (i *Index) Buckets(ctx context.Context, fn func(bucket string) error) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "scan buckets")
	}

	lower := []byte{keyspace.Usage}

	iter, err := i.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: keyspace.UpperBound(lower),
	})
	if err != nil {
		return errors.Wrap(err, "open index iterator")
	}

	defer func() { _ = iter.Close() }()

	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan buckets")
		}

		// The key is 'u' + bucket, and iter.Key() is only valid until the next
		// Next — so the name is copied before fn can retain it.
		if err := fn(string(iter.Key()[1:])); err != nil {
			return err
		}
	}

	if err := iter.Error(); err != nil {
		return errors.Wrap(err, "scan buckets")
	}

	return nil
}

// Reset implements metastore.Store, for a rebuild that must not inherit stale
// entries — an object deleted while the index was not watching leaves one
// behind, and nothing in a rebuild would otherwise remove it.
func (i *Index) Reset(ctx context.Context) error {
	if err := i.MarkBuilding(ctx); err != nil {
		return err
	}

	// Everything except the state marker, which was just written.
	for _, prefix := range [][]byte{{keyspace.Object}, {keyspace.Usage}} {
		if err := i.db.DeleteRange(prefix, keyspace.UpperBound(prefix), pebble.Sync); err != nil {
			return errors.Wrap(err, "reset index")
		}
	}

	return nil
}

// DefaultDir is where a node keeps its index, given the storage root.
func DefaultDir(root string) string { return filepath.Join(root, "cluster", "index") }

// SetVerified implements metastore.Store.
//
// Verification is written apart from the entry itself because only the scrub
// knows it and only the write path knows the rest: an object re-indexed by a
// write keeps whatever the scrub recorded, and a scrub recording a check does
// not disturb the size or etag a write just stored.
//
// Objects the index does not hold are skipped rather than created. The index
// records what this node holds; a stamp for something it does not hold would
// be an entry with no object behind it.
func (i *Index) SetVerified(ctx context.Context, records []metastore.Verification) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "record verification")
	}

	batch := i.db.NewBatch()
	defer func() { _ = batch.Close() }()

	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "record verification")
		}

		l := i.lock(rec.Bucket)
		l.Lock()

		entry, found, err := i.get(rec.Bucket, rec.Key)
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

	if err := batch.Commit(i.write); err != nil {
		return errors.Wrap(err, "commit verification")
	}

	return nil
}

// Coverage implements metastore.Store, reporting how stale this node's
// verification is.
//
// This is the number worth watching, and the one nothing could answer before:
// counters of scrub work done say how busy the scrubber was, not whether it
// ever reached the objects at the back of a disk. A cycle that cannot keep up
// shows here as an Oldest that keeps receding, or a Never that never falls.
//
// It reads index entries only — no disk, no network — so it costs a local scan
// rather than the walk it reports on.
func (i *Index) Coverage(ctx context.Context) (metastore.Coverage, error) {
	var cov metastore.Coverage

	// Collected rather than swept inline, deliberately. Scanning each bucket
	// from inside the Buckets callback would hold the usage-prefix iterator
	// open across every object in the index, pinning the SSTables underneath it
	// against compaction for the length of the whole sweep. The bucket names
	// are the small part; materializing them first is what keeps the long part
	// iterator-free.
	var buckets []string

	if err := i.Buckets(ctx, func(bucket string) error {
		buckets = append(buckets, bucket)

		return nil
	}); err != nil {
		return metastore.Coverage{}, err
	}

	for _, bucket := range buckets {
		err := i.Scan(ctx, bucket, "", "", 0, func(e metastore.Entry) error {
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
