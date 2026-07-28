package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	"go.uber.org/zap"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// Object commit records are "obj/<hash>/<hash>/meta". Both halves matter: the
// coordinator keeps bucket records under "bkt/" and ends those with "/meta"
// too, so matching the suffix alone feeds bucket records to an index that only
// understands objects — where they fail to decode and, worse, count as records
// the index missed, which is the signal that it has fallen behind.
const (
	objectPrefix  = "obj/"
	sidecarSuffix = "/meta"
)

// isObjectRecord reports whether a name is an object's commit record.
func isObjectRecord(name string) bool {
	return strings.HasPrefix(name, objectPrefix) && strings.HasSuffix(name, sidecarSuffix)
}

// objectIndexer keeps a node's object index in step with what lands on its
// disks.
//
// Every write to this node's disks passes through the store, whichever node's
// coordinator issued it — its own, or a peer placing a replica here — so this
// is the one seam that sees the node's whole object set. Watching the local
// coordinator instead would index the objects this node *wrote*, which is a
// different and wrong set.
//
// Failures are counted, not returned. The record is already durable and the
// index is derived: dropping an entry costs a rebuild, while failing the write
// that produced it would cost the object.
type objectIndexer struct {
	index metastore.Store
	lg    *zap.Logger

	// dropped counts records the index could not take. A non-zero count means
	// the index is behind and a rebuild is owed.
	dropped atomic.Int64
}

var _ diskstore.CommitObserver = (*objectIndexer)(nil)

func newObjectIndexer(index metastore.Store, lg *zap.Logger) *objectIndexer {
	return &objectIndexer{index: index, lg: lg}
}

// observed is the context the observer records under.
//
// diskstore.CommitObserver carries none, and inheriting the write's would be
// wrong even if it did: the record is already durable by the time this runs, so
// a caller who has given up must not also take the index entry for what its
// disks now hold. The store's own timeouts bound the call.
func observed() context.Context { return context.Background() }

// Wants implements diskstore.CommitObserver: only commit records carry object
// identity, so only they are worth reading back. Payload fragments are named
// by generation and hold no metadata at all.
func (o *objectIndexer) Wants(name string) bool { return isObjectRecord(name) }

// Committed implements diskstore.CommitObserver.
func (o *objectIndexer) Committed(disk cluster.DiskID, name string, data []byte) {
	entry, err := indexEntry(disk, data)
	if err != nil {
		o.drop(name, err)

		return
	}

	if err := o.index.Put(observed(), entry); err != nil {
		o.drop(name, err)
	}
}

// Deleted implements diskstore.CommitObserver.
func (o *objectIndexer) Deleted(_ cluster.DiskID, name string, data []byte) {
	var sc clusterstore.Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		o.drop(name, err)

		return
	}

	if err := o.index.Delete(observed(), sc.Bucket, sc.Key); err != nil {
		o.drop(name, err)
	}
}

// drop records that the index missed a record.
func (o *objectIndexer) drop(name string, err error) {
	o.dropped.Add(1)
	o.lg.Warn("Object index missed a record; a rebuild will recover it",
		zap.String("name", name), zap.Error(err))
}

// Dropped reports how many records the index failed to take since start.
func (o *objectIndexer) Dropped() int64 { return o.dropped.Load() }

// indexEntry turns a stored commit record into an index entry.
func indexEntry(disk cluster.DiskID, data []byte) (metastore.Entry, error) {
	var sc clusterstore.Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return metastore.Entry{}, errors.Wrap(err, "decode commit record")
	}

	if sc.Bucket == "" || sc.Key == "" {
		return metastore.Entry{}, errors.New("commit record names no object")
	}

	return metastore.Entry{
		Bucket:     sc.Bucket,
		Key:        sc.Key,
		Size:       sc.Size,
		ETag:       sc.ETag,
		Modified:   sc.Modified,
		Seq:        sc.Seq,
		Generation: sc.Generation,
		Disk:       disk,
		OwnerID:    sc.Owner.ID,
		OwnerName:  sc.Owner.DisplayName,
	}, nil
}

// readAllClose drains a record and closes it.
func readAllClose(rc io.ReadCloser) ([]byte, error) {
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, errors.Wrap(err, "read commit record")
	}

	return data, nil
}

// indexPages answers a peer's index query from this node's index.
//
// A node whose index is not ready answers so rather than answering short: its
// objects would simply be missing from the merge, and the caller reads the
// sidecars instead. Reporting an empty page as if it were complete is how a
// listing silently loses keys.
func (rt *clusterRuntime) indexPages(ctx context.Context, q transport.IndexQuery) (transport.IndexPage, error) {
	if rt.index == nil {
		return transport.IndexPage{}, nil
	}

	state, err := rt.index.State(ctx)
	if err != nil || state != metastore.StateReady {
		return transport.IndexPage{}, nil //nolint:nilerr // Not ready is an answer, not a failure.
	}

	page := transport.IndexPage{Ready: true}

	err = rt.index.Scan(ctx, q.Bucket, q.Prefix, q.After, q.Limit, func(e metastore.Entry) error {
		page.Entries = append(page.Entries, transport.IndexEntry{
			Key:        e.Key,
			Size:       e.Size,
			ETag:       e.ETag,
			Modified:   e.Modified,
			Seq:        e.Seq,
			Generation: e.Generation,
			OwnerID:    e.OwnerID,
			OwnerName:  e.OwnerName,
		})

		return nil
	})
	if err != nil {
		return transport.IndexPage{}, errors.Wrap(err, "scan index")
	}

	return page, nil
}

// buildObjectIndex rebuilds a node's index from its own disks.
//
// This is the expensive, authoritative half: it streams every name on every
// disk and reads each commit record, which is the same walk the scrub makes.
// It runs when the index was never built or was not handed over cleanly, and
// it is what makes the index safe to keep unsynced — anything a crash swallowed
// is recovered from the disks that still hold it.
//
// The index is emptied first. A rebuild that only added would keep entries for
// objects deleted while nothing was watching, and an index that lists what is
// gone is worse than one that is merely behind.
func (rt *clusterRuntime) buildObjectIndex(ctx context.Context) error {
	if rt.index == nil {
		return nil
	}

	started := time.Now()

	if err := rt.index.Reset(ctx); err != nil {
		return err
	}

	var objects int64

	for _, disk := range rt.node.Disks {
		err := rt.store.WalkFragments(ctx, disk.ID, "", func(name string) error {
			if !isObjectRecord(name) {
				return nil
			}

			rc, _, err := rt.store.Open(ctx, disk.ID, name)
			if err != nil {
				// Raced a delete, or one unreadable record: neither is worth
				// abandoning the build for.
				return nil //nolint:nilerr // A missing record is not a build failure.
			}

			data, err := readAllClose(rc)
			if err != nil {
				return nil //nolint:nilerr // Same.
			}

			entry, err := indexEntry(disk.ID, data)
			if err != nil {
				return nil //nolint:nilerr // Same.
			}

			if err := rt.index.Put(ctx, entry); err != nil {
				return err
			}

			objects++

			return nil
		})
		if err != nil {
			return errors.Wrapf(err, "index disk %s", disk.ID)
		}
	}

	if err := rt.index.MarkReady(ctx); err != nil {
		return err
	}

	rt.lg.Info("Object index built",
		zap.Int64("objects", objects),
		zap.Duration("took", time.Since(started)),
	)

	return nil
}

// RunObjectIndex brings the index up to date at startup and leaves it to the
// write path from there.
//
// Nothing reads the index yet — listings, usage and the scrub still take their
// own walks — so a build that fails costs nothing but the next attempt. When
// readers arrive they will consult State first, the way the occupancy counters
// report "not counted yet" rather than a confident zero.
func (rt *clusterRuntime) RunObjectIndex(ctx context.Context) {
	if rt.index == nil {
		return
	}

	if rt.indexAdopted {
		// The previous process closed cleanly, so the entries are trustworthy
		// and the walk below would re-derive what is already there. Marking
		// ready is not optional: Open leaves every index building so that a
		// crash schedules a rebuild, and a node that adopts without saying so
		// stays excluded from listing merges for as long as it runs.
		if err := rt.index.MarkReady(ctx); err != nil {
			rt.lg.Warn("Adopting the object index failed; rebuilding instead", zap.Error(err))
		} else {
			rt.lg.Info("Object index adopted from a clean shutdown")

			return
		}
	}

	if err := rt.buildObjectIndex(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}

		rt.lg.Warn("Object index build failed; it stays unusable until the next start", zap.Error(err))
	}
}

// verifyBatch is how many verification stamps accumulate before they are
// written. Batching is the point: a scrub verifying millions of objects should
// not pay a durable write for each, and losing the last few to a crash costs
// re-verifying those objects — which is the cheapest possible thing to lose.
const verifyBatch = 512

// objectVerifier records what the scrub has checked into the node's index, and
// answers what it has.
//
// The pending batch doubles as the pass's memory of what it has already swept.
// That is what lets the scrub drop the set it used to keep of every object in
// a pass: the set grew with the node, and this is bounded by the batch.
type objectVerifier struct {
	index metastore.Store
	lg    *zap.Logger

	mu      sync.Mutex
	pending map[string]time.Time
}

var _ clusterstore.VerificationIndex = (*objectVerifier)(nil)

func newObjectVerifier(index metastore.Store, lg *zap.Logger) *objectVerifier {
	return &objectVerifier{index: index, lg: lg, pending: make(map[string]time.Time)}
}

// verifyKey is how a pending stamp is keyed. Bucket names cannot contain a NUL
// and object keys cannot carry one, which is what the coordinator's own object
// references rest on.
func verifyKey(bucket, key string) string { return bucket + "\x00" + key }

// LastVerified implements clusterstore.VerificationIndex, answering from the
// unwritten batch first: a stamp recorded moments ago has not reached the index
// yet, and missing it would sweep the same object twice in one pass.
func (v *objectVerifier) LastVerified(bucket, key string) (time.Time, bool) {
	v.mu.Lock()
	at, pending := v.pending[verifyKey(bucket, key)]
	v.mu.Unlock()

	if pending {
		return at, true
	}

	entry, found, err := v.index.Get(observed(), bucket, key)
	if err != nil || !found || entry.VerifiedAt.IsZero() {
		return time.Time{}, false
	}

	return entry.VerifiedAt, true
}

// RecordVerified implements clusterstore.VerificationIndex.
func (v *objectVerifier) RecordVerified(bucket, key string, at time.Time) {
	v.mu.Lock()

	v.pending[verifyKey(bucket, key)] = at
	full := len(v.pending) >= verifyBatch

	v.mu.Unlock()

	if !full {
		return
	}

	if err := v.Flush(); err != nil {
		v.lg.Warn("Recording scrub verification failed; those objects re-verify next pass", zap.Error(err))
	}
}

// Flush implements clusterstore.VerificationIndex.
func (v *objectVerifier) Flush() error {
	v.mu.Lock()

	if len(v.pending) == 0 {
		v.mu.Unlock()

		return nil
	}

	records := make([]metastore.Verification, 0, len(v.pending))

	for k, at := range v.pending {
		bucket, key, ok := strings.Cut(k, "\x00")
		if !ok {
			continue
		}

		records = append(records, metastore.Verification{Bucket: bucket, Key: key, At: at})
	}

	v.pending = make(map[string]time.Time)

	v.mu.Unlock()

	return v.index.SetVerified(observed(), records)
}
