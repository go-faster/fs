package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	"go.uber.org/zap"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
	"github.com/go-faster/fs/internal/cluster/objindex"
)

// sidecarSuffix marks the object commit records among a disk's names. The
// coordinator mints them; this is the one place the node's wiring has to know
// the shape, because the store below deals in opaque names and the index above
// deals in objects.
const sidecarSuffix = "/meta"

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
	index *objindex.Index
	lg    *zap.Logger

	// dropped counts records the index could not take. A non-zero count means
	// the index is behind and a rebuild is owed.
	dropped atomic.Int64
}

var _ diskstore.CommitObserver = (*objectIndexer)(nil)

func newObjectIndexer(index *objindex.Index, lg *zap.Logger) *objectIndexer {
	return &objectIndexer{index: index, lg: lg}
}

// Wants implements diskstore.CommitObserver: only commit records carry object
// identity, so only they are worth reading back. Payload fragments are named
// by generation and hold no metadata at all.
func (o *objectIndexer) Wants(name string) bool { return strings.HasSuffix(name, sidecarSuffix) }

// Committed implements diskstore.CommitObserver.
func (o *objectIndexer) Committed(disk cluster.DiskID, name string, data []byte) {
	entry, err := indexEntry(disk, data)
	if err != nil {
		o.drop(name, err)

		return
	}

	if err := o.index.Put(entry); err != nil {
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

	if err := o.index.Delete(sc.Bucket, sc.Key); err != nil {
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
func indexEntry(disk cluster.DiskID, data []byte) (objindex.Entry, error) {
	var sc clusterstore.Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return objindex.Entry{}, errors.Wrap(err, "decode commit record")
	}

	if sc.Bucket == "" || sc.Key == "" {
		return objindex.Entry{}, errors.New("commit record names no object")
	}

	return objindex.Entry{
		Bucket:     sc.Bucket,
		Key:        sc.Key,
		Size:       sc.Size,
		ETag:       sc.ETag,
		Modified:   sc.Modified,
		Seq:        sc.Seq,
		Generation: sc.Generation,
		Disk:       disk,
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

	if err := rt.index.Reset(); err != nil {
		return err
	}

	var objects int64

	for _, disk := range rt.node.Disks {
		err := rt.store.WalkFragments(ctx, disk.ID, "", func(name string) error {
			if !strings.HasSuffix(name, sidecarSuffix) {
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

			if err := rt.index.Put(entry); err != nil {
				return err
			}

			objects++

			return nil
		})
		if err != nil {
			return errors.Wrapf(err, "index disk %s", disk.ID)
		}
	}

	if err := rt.index.MarkReady(); err != nil {
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

	state, err := rt.index.State()
	if err != nil {
		rt.lg.Warn("Object index state unreadable; rebuilding", zap.Error(err))

		state = objindex.StateBuilding
	}

	if state == objindex.StateReady {
		rt.lg.Debug("Object index adopted from a clean shutdown")

		return
	}

	if err := rt.buildObjectIndex(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}

		rt.lg.Warn("Object index build failed; it stays unusable until the next start", zap.Error(err))
	}
}
