package clusterstore

import (
	"context"
	"encoding/json"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/metastore"
)

// RebuildCursor is how far a cluster-wide metadata rebuild has got: everything
// at or before this key, in this bucket, and every bucket sorting before it, is
// already in the store.
type RebuildCursor struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// Encode serializes the cursor for persistence (etcd).
func (c RebuildCursor) Encode() (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", errors.Wrap(err, "encode rebuild cursor")
	}

	return string(data), nil
}

// DecodeRebuildCursor parses a persisted cursor.
func DecodeRebuildCursor(raw string) (RebuildCursor, error) {
	var c RebuildCursor

	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return RebuildCursor{}, errors.Wrap(err, "decode rebuild cursor")
	}

	return c, nil
}

// rebuildBatch is how many objects are indexed between checkpoints. Small
// enough that a killed runner repeats little, large enough that the checkpoint
// is not the cost of the walk.
const rebuildBatch = 500

// RebuildOptions configures a cluster-wide metadata rebuild.
type RebuildOptions struct {
	// Resume is where a previous runner stopped. Zero starts from the
	// beginning — and, crucially, starts a *new* rebuild rather than
	// continuing one. See RebuildMetadata.
	Resume RebuildCursor
	// Resuming reports that Resume came from a persisted cursor rather than
	// from nowhere. It is separate from Resume being zero because a rebuild
	// legitimately checkpoints at the first bucket's first key, and a runner
	// that died immediately afterwards must still resume rather than restart.
	Resuming bool
	// Checkpoint persists the cursor. Called after each batch, and its error
	// stops the rebuild: a walk that cannot record progress would repeat all of
	// it after a kill, which is the failure the cursor exists to prevent.
	Checkpoint func(ctx context.Context, cur RebuildCursor) error
	// OnObject observes progress. May be nil.
	OnObject func(bucket, key string)
}

// RebuildReport is what a rebuild covered.
type RebuildReport struct {
	Buckets int
	Objects int
}

// RebuildMetadata refills a cluster-scope store from the sidecars on the disks.
//
// # Why this is not the local-scope rebuild
//
// At local scope a node rebuilds its own index from its own disks: self-contained,
// needing no coordination, and the recovery story the sizing doc already
// publishes. A cluster-scope store describes objects no single node holds, so it
// cannot be rebuilt by any node from what it can see locally. It becomes one
// cooperative walk of the whole cluster, which needs exactly one runner, a
// cursor so a killed runner does not start over, and a story for what a standby
// picks up. That is the real cost of cluster scope and it belongs here rather
// than in a footnote.
//
// # Reset happens once per rebuild, not once per runner
//
// The store is emptied first, because a rebuild that only added would keep
// entries for objects deleted while nothing was watching, and a store listing
// what is gone is worse than one merely behind.
//
// But emptying is *not* idempotent across resumes. A standby that took over
// after the elected runner died must continue from the cursor, and resetting
// again would discard everything the dead runner had already written — turning
// every leadership change into a restart and, on a cluster large enough to
// matter, into a rebuild that never finishes. Opts.Resuming is what separates
// the two cases, and it is a separate field from a zero cursor precisely
// because checkpointing at the very first key is legitimate.
//
// The sidecars are read directly rather than through the listing path, for the
// obvious reason: the store being rebuilt cannot be the source of truth for
// rebuilding itself.
//
// The target store is a parameter rather than the coordinator's own. A rebuild
// is "walk the cluster and fill this", and which store that is belongs to the
// caller — the elected runner knows, the coordinator does not need to. It also
// keeps the dependency visible: a walk that silently filled whatever the
// coordinator happened to hold would be a surprise the first time those two
// were not the same.
func (c *Coordinator) RebuildMetadata(
	ctx context.Context,
	store metastore.Store,
	opts RebuildOptions,
) (*RebuildReport, error) {
	if store == nil || store.Scope() != metastore.ScopeCluster {
		return nil, errors.New("clusterstore: a cluster-scope metastore is required to rebuild")
	}

	if opts.Resuming {
		// Already emptied by whoever started this rebuild. Re-marking is
		// harmless and covers a store that was somehow left ready.
		if err := store.MarkBuilding(ctx); err != nil {
			return nil, err
		}
	} else if err := store.Reset(ctx); err != nil {
		return nil, err
	}

	buckets, err := c.ListBuckets(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list buckets")
	}

	report := &RebuildReport{}
	pending := 0

	for _, b := range buckets {
		if internalBucket(b.Name) || b.Name < opts.Resume.Bucket {
			continue
		}

		// The authoritative read: every sidecar on every disk, replicas of one
		// object collapsed to the newest record by the sidecars' own order.
		objects, err := c.ListObjects(ctx, b.Name, "")
		if err != nil {
			return report, errors.Wrapf(err, "walk bucket %q", b.Name)
		}

		// Resume inside the cursor's bucket: everything at or before the key is
		// already in the store. ListObjects returns keys sorted.
		if b.Name == opts.Resume.Bucket {
			for len(objects) > 0 && objects[0].Key <= opts.Resume.Key {
				objects = objects[1:]
			}
		}

		report.Buckets++

		for _, sc := range objects {
			if err := ctx.Err(); err != nil {
				return report, err
			}

			if err := store.Put(ctx, metastore.Entry{
				Bucket:     sc.Bucket,
				Key:        sc.Key,
				Size:       sc.Size,
				ETag:       sc.ETag,
				Modified:   sc.Modified,
				Seq:        sc.Seq,
				Generation: sc.Generation,
				OwnerID:    sc.Owner.ID,
				OwnerName:  sc.Owner.DisplayName,
			}); err != nil {
				return report, errors.Wrapf(err, "index %q/%q", sc.Bucket, sc.Key)
			}

			report.Objects++
			pending++

			if opts.OnObject != nil {
				opts.OnObject(sc.Bucket, sc.Key)
			}

			if pending >= rebuildBatch {
				if err := checkpoint(ctx, opts, RebuildCursor{Bucket: sc.Bucket, Key: sc.Key}); err != nil {
					return report, err
				}

				pending = 0
			}
		}

		// Checkpoint at each bucket boundary too, so a kill between buckets
		// does not re-walk the one just finished.
		if len(objects) > 0 {
			last := objects[len(objects)-1]
			if err := checkpoint(ctx, opts, RebuildCursor{Bucket: last.Bucket, Key: last.Key}); err != nil {
				return report, err
			}

			pending = 0
		}
	}

	// Only now is the store worth reading. Marking ready before the walk
	// finished would serve a listing that reports a fraction of the cluster as
	// all of it — the one outcome worse than refusing to answer.
	if err := store.MarkReady(ctx); err != nil {
		return report, err
	}

	return report, nil
}

// checkpoint records progress, when a caller asked to be told.
func checkpoint(ctx context.Context, opts RebuildOptions, cur RebuildCursor) error {
	if opts.Checkpoint == nil {
		return nil
	}

	if err := opts.Checkpoint(ctx, cur); err != nil {
		return errors.Wrap(err, "checkpoint rebuild cursor")
	}

	return nil
}
