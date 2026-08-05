package shardstore

import (
	"context"
	"encoding/json"

	"github.com/cockroachdb/pebble/v2"
	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/keyspace"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// DefaultBackfillBatch is how many entries one backfill step carries.
//
// Small enough that a step is a short request rather than a transfer, because
// the cursor only advances between steps: a killed backfill repeats at most one
// step's work, and a step that carried a whole range would repeat a whole range.
const DefaultBackfillBatch = 512

// Learn stores entries into a range this shard replicates but does not own.
//
// # Why this is not ApplyBatch
//
// ApplyBatch replays an owner's pebble batch verbatim, which is exactly right
// for log shipping — the owner's state becomes the replica's state, with
// nothing reconciled. It is wrong for a backfill, because a backfill and the
// log are two writers racing over the same keys:
//
//	backfill reads k at V1 → a client writes k, the log ships V2 → the
//	backfill's batch lands, writing V1 over V2
//
// A lost update, and a silent one: the replica looks complete and holds a stale
// value that would survive promotion. So a backfilled entry goes through the
// same supersede rules an ordinary write does, and an older record loses.
//
// # Which makes the backfill resumable
//
// Once order between the backfill and the log stops mattering, a cursor can be
// replayed over keys already copied with no effect. That is what lets a killed
// backfill resume from a persisted position rather than from knowing where the
// log was at the time — which nothing records and nothing could.
func (s *Shard) Learn(ctx context.Context, r rangemap.Range, entries []metastore.Entry) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(err, "learn entries")
	}

	if !s.learns(r) {
		return ErrNotLearned
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "learn entries")
		}

		// An entry outside the range is one this shard was never told to hold,
		// and storing it would leave data nothing asks for and nothing cleans
		// up — the same reason ApplyBatch refuses a range it does not follow.
		if !r.Contains(string(keyspace.ObjectKey(e.Bucket, e.Key))) {
			return errors.Errorf("entry %s/%s is outside the range being backfilled", e.Bucket, e.Key)
		}

		// Synced, like ApplyBatch and unlike the owner's own writes. A replica
		// exists to be current when the owner is gone; one that lost its tail
		// to a crash would be promoted into exactly the gap it was there to
		// prevent — and a backfill that had to start over is the expensive
		// outcome this whole path exists to avoid. (Durability, so no test
		// reaches it: only a crash tells the two apart.)
		//
		// Nothing ships. A learner echoing its backfill back to the node it
		// came from is an amplification loop, and shipTo already declines for a
		// range the shard does not own — passing no callback is the second
		// lock on a door that is shut, kept because the first one is somewhere
		// else and could move.
		if err := s.store(e, pebble.Sync, nil); err != nil {
			return err
		}
	}

	return nil
}

// BackfillStep is what one step of a backfill copied.
type BackfillStep struct {
	// Entries are the records read, in key order.
	Entries []metastore.Entry
	// Cursor is where the next step resumes: the key after the last one read.
	// Empty when the range is exhausted.
	Cursor string
	// Done reports that the range has been walked to its end.
	Done bool
}

// ReadBackfill reads one step of a range from this shard, resuming after
// cursor.
//
// The owner's half of a move. It reads and reports; shipping is the caller's,
// so the same step can be retried against a different node without the shard
// knowing a move exists.
//
// A step is bounded rather than streamed, because the cursor only advances
// between steps: a killed backfill repeats at most one step, and a step that
// carried the whole range would repeat the whole range.
func (s *Shard) ReadBackfill(
	ctx context.Context,
	r rangemap.Range,
	cursor string,
	limit int,
) (BackfillStep, error) {
	if err := ctx.Err(); err != nil {
		return BackfillStep{}, errors.Wrap(err, "read backfill")
	}

	if !s.serves(r) {
		return BackfillStep{}, ErrNotOwned
	}

	if limit <= 0 {
		limit = DefaultBackfillBatch
	}

	from := []byte(r.Start)
	if cursor != "" {
		from = []byte(cursor)
	}

	// Bounded by the range and by the object prefix together. Without the
	// second, a walk of the last range would run into the usage counters, which
	// sort above every object key and are not entries at all.
	upper := upperOf(r)
	if upper == nil || string(upper) > string(objectUpperBound) {
		upper = objectUpperBound
	}

	if string(from) < string(objectLowerBound) {
		from = objectLowerBound
	}

	step := BackfillStep{Done: true}

	if string(from) >= string(upper) {
		return step, nil
	}

	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: from, UpperBound: upper})
	if err != nil {
		return BackfillStep{}, errors.Wrap(err, "open backfill iterator")
	}

	defer func() { _ = iter.Close() }()

	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			return BackfillStep{}, errors.Wrap(err, "read backfill")
		}

		if len(step.Entries) >= limit {
			// Stopped short, so the range is not done and the next step starts
			// where this one gave up.
			step.Done = false
			step.Cursor = string(iter.Key())

			break
		}

		var e metastore.Entry
		if err := json.Unmarshal(iter.Value(), &e); err != nil {
			// A record this shard cannot read is one the owner will not send.
			// Skipped rather than fatal: it is already unreachable here, and
			// failing the move would make one bad record permanent.
			continue
		}

		step.Entries = append(step.Entries, e)
	}

	if err := iter.Error(); err != nil {
		return BackfillStep{}, errors.Wrap(err, "read backfill")
	}

	return step, nil
}

// Object key bounds, so a backfill walks entries and nothing else.
var (
	objectLowerBound = []byte{keyspace.Object}
	objectUpperBound = keyspace.UpperBound([]byte{keyspace.Object})
)
