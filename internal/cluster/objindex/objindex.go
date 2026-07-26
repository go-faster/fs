// Package objindex is a node's local index of the objects it holds.
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
package objindex

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"path/filepath"
	"sync"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// Key prefixes. One byte each, so a scan of one kind never sees another.
const (
	prefixObject = 'o'
	prefixUsage  = 'u'
	prefixMeta   = 'm'
)

// stateKey holds whether the index was handed over cleanly by the last
// process.
var stateKey = []byte{prefixMeta, 's'}

// State is what the index believes about itself.
type State uint8

const (
	// StateBuilding means the index is not usable: it was never built, or the
	// last process did not close it and anything written since its last flush
	// may be missing. Either way a full rebuild is owed.
	StateBuilding State = iota
	// StateReady means a build completed and every process since handed the
	// index over cleanly.
	StateReady
)

// Entry is one object as this node holds it.
type Entry struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	ETag   string `json:"etag,omitempty"`
	// Modified, Seq and Generation carry the sidecar's ordering stamps, so the
	// index can tell a newer record from an older one arriving late — a
	// rebalance copying a superseded generation, say — and refuse to go
	// backwards.
	Modified   time.Time `json:"modified"`
	Seq        int64     `json:"seq,omitempty"`
	Generation string    `json:"generation,omitempty"`
	// Disk is where the newest record was seen. A node can hold copies of one
	// object on two disks under different epochs; the index keeps one entry
	// per object, naming the disk the winning record came from.
	Disk cluster.DiskID `json:"disk,omitempty"`
	// VerifiedAt is when the scrub last checked this object's payload. Zero
	// means never, which is how a sweep finds what to do first.
	VerifiedAt time.Time `json:"verified_at,omitzero"`
}

// supersedes reports whether e is newer than other, by the same total order
// the sidecars use: sequence first, then write time, then the generation
// stamp as a deterministic tie-break.
func (e Entry) supersedes(other Entry) bool {
	if e.Seq != other.Seq {
		return e.Seq > other.Seq
	}

	if !e.Modified.Equal(other.Modified) {
		return e.Modified.After(other.Modified)
	}

	return e.Generation > other.Generation
}

// Usage is what a bucket holds on this node.
type Usage struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

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
}

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

	state, err := idx.State()
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	// Invalidate before serving a single write: from here on only an orderly
	// Close can call the index ready.
	if state == StateReady {
		if err := idx.setState(StateBuilding); err != nil {
			_ = db.Close()

			return nil, err
		}
	}

	return idx, nil
}

// Close records the index as ready and closes it. An index that is not closed
// is not damaged — it is rebuilt.
func (i *Index) Close() error {
	var err error

	i.once.Do(func() {
		if serr := i.setState(StateReady); serr != nil {
			err = serr
		}

		if cerr := i.db.Close(); cerr != nil && err == nil {
			err = errors.Wrap(cerr, "close index")
		}
	})

	return err
}

// Dir is where the index lives.
func (i *Index) Dir() string { return i.dir }

// State reports whether the index can be trusted.
func (i *Index) State() (State, error) {
	value, closer, err := i.db.Get(stateKey)
	if errors.Is(err, pebble.ErrNotFound) {
		return StateBuilding, nil
	}

	if err != nil {
		return StateBuilding, errors.Wrap(err, "read index state")
	}

	defer func() { _ = closer.Close() }()

	if len(value) == 1 && value[0] == byte(StateReady) {
		return StateReady, nil
	}

	return StateBuilding, nil
}

// MarkReady records that a build finished. Close records it too; this is for a
// build that completes while the process keeps running.
func (i *Index) MarkReady() error { return i.setState(StateReady) }

// MarkBuilding records that the index is being rebuilt and must not be
// trusted until it is not.
func (i *Index) MarkBuilding() error { return i.setState(StateBuilding) }

func (i *Index) setState(s State) error {
	if err := i.db.Set(stateKey, []byte{byte(s)}, pebble.Sync); err != nil {
		return errors.Wrap(err, "write index state")
	}

	return nil
}

// objectKey is 'o' + bucket + NUL + key, so a scan of a bucket's objects is a
// range scan in key order. NUL separates because no S3 bucket name may contain
// one and no object key survives XML with one, which is the same reasoning the
// coordinator's object references already rest on.
func objectKey(bucket, key string) []byte {
	out := make([]byte, 0, 2+len(bucket)+len(key))
	out = append(out, prefixObject)
	out = append(out, bucket...)
	out = append(out, 0)
	out = append(out, key...)

	return out
}

// usageKey is 'u' + bucket.
func usageKey(bucket string) []byte {
	out := make([]byte, 0, 1+len(bucket))
	out = append(out, prefixUsage)
	out = append(out, bucket...)

	return out
}

// bucketPrefix is every object key of a bucket: 'o' + bucket + NUL.
func bucketPrefix(bucket string) []byte {
	out := make([]byte, 0, 2+len(bucket))
	out = append(out, prefixObject)
	out = append(out, bucket...)
	out = append(out, 0)

	return out
}

// upperBound is the exclusive end of a prefix scan: the prefix with its last
// byte incremented. A prefix of all 0xFF has no successor, and nil then means
// "to the end", which is correct because nothing sorts above it.
func upperBound(prefix []byte) []byte {
	out := bytes.Clone(prefix)

	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xFF {
			out[i]++

			return out[:i+1]
		}
	}

	return nil
}

// lock returns the stripe guarding a bucket's updates.
func (i *Index) lock(bucket string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucket))

	return &i.mu[h.Sum32()%stripes]
}

// Put records an object, adjusting the bucket's counters by what it displaces.
//
// A record that does not supersede the stored one is dropped: an older sidecar
// can arrive after a newer one — a rebalance copying a superseded generation,
// a repair completing late — and letting it win would resurrect a stale size
// in both the entry and the counters.
func (i *Index) Put(e Entry) error {
	l := i.lock(e.Bucket)
	l.Lock()
	defer l.Unlock()

	prev, found, err := i.get(e.Bucket, e.Key)
	if err != nil {
		return err
	}

	if found && !e.supersedes(prev) {
		return nil
	}

	// A re-index of the same object keeps whatever the scrub last recorded;
	// only a scrub sets it, and it does not know about writes.
	if found && e.VerifiedAt.IsZero() {
		e.VerifiedAt = prev.VerifiedAt
	}

	delta := Usage{Objects: 1, Bytes: e.Size}
	if found {
		delta = Usage{Bytes: e.Size - prev.Size}
	}

	data, err := json.Marshal(e)
	if err != nil {
		return errors.Wrap(err, "marshal index entry")
	}

	batch := i.db.NewBatch()
	defer func() { _ = batch.Close() }()

	if err := batch.Set(objectKey(e.Bucket, e.Key), data, nil); err != nil {
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

// Delete removes an object and takes its bytes back out of the bucket's
// counters. Removing what is not there is not an error.
func (i *Index) Delete(bucket, key string) error {
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

	if err := batch.Delete(objectKey(bucket, key), nil); err != nil {
		return errors.Wrap(err, "stage index delete")
	}

	if err := i.stageUsage(batch, bucket, Usage{Objects: -1, Bytes: -prev.Size}); err != nil {
		return err
	}

	if err := batch.Commit(i.write); err != nil {
		return errors.Wrap(err, "commit index delete")
	}

	return nil
}

// stageUsage folds a delta into the bucket's counters within batch. The caller
// holds the bucket's stripe, so the read cannot race another update.
func (i *Index) stageUsage(batch *pebble.Batch, bucket string, delta Usage) error {
	usage, err := i.Usage(bucket)
	if err != nil {
		return err
	}

	usage.Objects = max(usage.Objects+delta.Objects, 0)
	usage.Bytes = max(usage.Bytes+delta.Bytes, 0)

	data, err := json.Marshal(usage)
	if err != nil {
		return errors.Wrap(err, "marshal usage")
	}

	if err := batch.Set(usageKey(bucket), data, nil); err != nil {
		return errors.Wrap(err, "stage usage")
	}

	return nil
}

// Get returns one object's entry.
func (i *Index) Get(bucket, key string) (Entry, bool, error) { return i.get(bucket, key) }

func (i *Index) get(bucket, key string) (Entry, bool, error) {
	value, closer, err := i.db.Get(objectKey(bucket, key))
	if errors.Is(err, pebble.ErrNotFound) {
		return Entry{}, false, nil
	}

	if err != nil {
		return Entry{}, false, errors.Wrap(err, "read index entry")
	}

	defer func() { _ = closer.Close() }()

	var e Entry
	if err := json.Unmarshal(value, &e); err != nil {
		return Entry{}, false, errors.Wrap(err, "unmarshal index entry")
	}

	return e, true, nil
}

// Usage returns a bucket's counters as this node holds them.
func (i *Index) Usage(bucket string) (Usage, error) {
	value, closer, err := i.db.Get(usageKey(bucket))
	if errors.Is(err, pebble.ErrNotFound) {
		return Usage{}, nil
	}

	if err != nil {
		return Usage{}, errors.Wrap(err, "read usage")
	}

	defer func() { _ = closer.Close() }()

	var u Usage
	if err := json.Unmarshal(value, &u); err != nil {
		return Usage{}, errors.Wrap(err, "unmarshal usage")
	}

	return u, nil
}

// Scan calls fn for each of a bucket's objects whose key starts with prefix
// and sorts after `after`, in key order, stopping after limit entries or when
// fn returns an error.
//
// This is the shape a listing needs and the reason for the whole package: the
// answer costs what the page contains rather than what the bucket holds.
// Nothing reads it yet — the listing path arrives with the peer RPC.
func (i *Index) Scan(bucket, prefix, after string, limit int, fn func(Entry) error) error {
	lower := bucketPrefix(bucket)
	if prefix != "" {
		lower = append(lower, prefix...)
	}

	// Start past the boundary when one is given and it is inside the range.
	start := lower
	if after != "" {
		if candidate := append(bucketPrefix(bucket), after...); bytes.Compare(candidate, lower) >= 0 {
			start = append(candidate, 0)
		}
	}

	iter, err := i.db.NewIter(&pebble.IterOptions{
		LowerBound: start,
		UpperBound: upperBound(lower),
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

		var e Entry
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

// Buckets returns every bucket the index holds counters for, sorted.
func (i *Index) Buckets() ([]string, error) {
	lower := []byte{prefixUsage}

	iter, err := i.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upperBound(lower),
	})
	if err != nil {
		return nil, errors.Wrap(err, "open index iterator")
	}

	defer func() { _ = iter.Close() }()

	var out []string

	for iter.First(); iter.Valid(); iter.Next() {
		out = append(out, string(iter.Key()[1:]))
	}

	if err := iter.Error(); err != nil {
		return nil, errors.Wrap(err, "scan buckets")
	}

	return out, nil
}

// Reset empties the index, for a rebuild that must not inherit stale entries —
// an object deleted while the index was not watching leaves one behind, and
// nothing in a rebuild would otherwise remove it.
func (i *Index) Reset() error {
	if err := i.MarkBuilding(); err != nil {
		return err
	}

	// Everything except the state marker, which was just written.
	for _, prefix := range [][]byte{{prefixObject}, {prefixUsage}} {
		if err := i.db.DeleteRange(prefix, upperBound(prefix), pebble.Sync); err != nil {
			return errors.Wrap(err, "reset index")
		}
	}

	return nil
}

// DefaultDir is where a node keeps its index, given the storage root.
func DefaultDir(root string) string { return filepath.Join(root, "cluster", "index") }
