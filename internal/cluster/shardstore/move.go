package shardstore

import (
	"context"
	"encoding/json"

	"github.com/cockroachdb/pebble/v2"
	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore/keyspace"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// Source is a shard a range can be read out of, one bounded step at a time.
//
// Narrow rather than part of Backend, the way Replica is: reading a range out
// wholesale is a replication concern, and a Store that could reach it would be
// one refactor away from serving a listing from a backfill.
type Source interface {
	ReadBackfill(ctx context.Context, r rangemap.Range, cursor string, limit int) (BackfillStep, error)
}

var (
	_ Source = (*Peer)(nil)
	_ Source = (*Shard)(nil)
)

// BackfillResult is what one run of a backfill got through.
type BackfillResult struct {
	// Steps is how many round trips it took, and Entries how many records they
	// carried. Both count this run only: a resumed backfill does not re-report
	// what an earlier one copied, because it does not read it again.
	Steps   int
	Entries int
	// Done reports that the range has been walked to its end — the learner now
	// holds everything the owner held, and the log is keeping it current.
	Done bool
}

// Backfill copies a range into this shard from the node that owns it, resuming
// where a previous run stopped.
//
// # The learner pulls
//
// The owner pushes its log and the learner pulls its backfill, which looks
// inconsistent and is not. The log is a stream of what the owner just did, and
// only the owner knows that. The backfill is a walk of what the owner already
// had, and the only thing that walk needs to remember is how far it has got —
// which is a fact about the learner. Keeping the cursor on the node whose data
// it describes is what makes the two impossible to disagree: a learner that
// lost its tail to a crash lost the cursor with it.
//
// # The cursor is written after the entries, never before
//
// A cursor is a claim that everything below it is already here. Persisting it
// before the entries land would make a crash in between turn that claim into a
// lie — a permanent hole in the middle of a range that reads as absent objects
// and that nothing would ever notice, because a range with a hole is a range
// that simply answers no.
//
// So a crash costs at most one step's work, which is the reason a step is
// bounded in the first place.
func (s *Shard) Backfill(
	ctx context.Context,
	r rangemap.Range,
	from Source,
	limit int,
) (BackfillResult, error) {
	if err := ctx.Err(); err != nil {
		return BackfillResult{}, errors.Wrap(err, "backfill range")
	}

	// The map is the instruction. A shard pulling a range it was not told to
	// learn would fill itself with data nothing routes to it — and Learn would
	// refuse every entry anyway, one step later and less clearly.
	if !s.learns(r) {
		return BackfillResult{}, ErrNotLearned
	}

	cursor, err := s.backfillCursor(r)
	if err != nil {
		return BackfillResult{}, err
	}

	var out BackfillResult

	for {
		if err := ctx.Err(); err != nil {
			return out, errors.Wrap(err, "backfill range")
		}

		step, err := from.ReadBackfill(ctx, r, cursor, limit)
		if err != nil {
			return out, errors.Wrap(err, "read a backfill step")
		}

		if err := s.Learn(ctx, r, step.Entries); err != nil {
			return out, errors.Wrap(err, "store a backfill step")
		}

		out.Steps++
		out.Entries += len(step.Entries)

		if step.Done {
			// Cleared rather than left pointing at the end, so no completed
			// cursor survives to be read by a later backfill of the same range.
			// A learner promoted, later moved away, and later still made a
			// learner of that range again would otherwise resume from a cursor
			// describing data it no longer has — and skip straight to serving a
			// range it never copied.
			if err := s.clearBackfillCursor(r); err != nil {
				return out, err
			}

			// Recorded only now, with the walk finished and every entry it
			// carried already stored. This is what a promotion is decided on.
			s.markCaughtUp(r)

			out.Done = true

			return out, nil
		}

		// A step that is not done must move, or this loop is infinite. The owner
		// has no way to produce such a step, which is exactly why it is checked:
		// the failure would be a node spinning against a peer forever, and
		// nothing about it would look like an error.
		if step.Cursor <= cursor {
			return out, errors.Errorf(
				"backfill cursor went from %q to %q without finishing the range",
				cursor, step.Cursor)
		}

		if err := s.setBackfillCursor(r, step.Cursor); err != nil {
			return out, err
		}

		cursor = step.Cursor
	}
}

// backfillState is what a learner remembers about a backfill in flight.
//
// The range's own bounds and owner are recorded alongside the position because
// a cursor is only meaningful against the range it was taken in. They are the
// check that makes a stale cursor cost work rather than correctness.
type backfillState struct {
	// End is the range's end, so a cursor taken in [a,c) is not reused for the
	// [a,b) a split left behind — that one is a different range that happens to
	// start in the same place.
	End string `json:"end"`
	// Owner is the node the copy was read from. A cursor claims that
	// [Start, At) matches the owner's contents; after a failover the promoted
	// node's contents may differ below At, and nothing distinguishes the records
	// it received from the ones it never did. Re-reading is bounded; a silently
	// stale prefix is not.
	Owner cluster.NodeID `json:"owner"`
	// At is the key the next step resumes from.
	At string `json:"at"`
}

// backfillCursor returns where a backfill of this range resumes, or the empty
// string to start from the beginning.
//
// A recorded position that does not match the range being copied is discarded
// rather than repaired. Starting over is one range's worth of reads that land
// on records already present and change nothing; guessing which part of a
// mismatched cursor still applies is how a hole gets left behind.
func (s *Shard) backfillCursor(r rangemap.Range) (string, error) {
	data, closer, err := s.db.Get(backfillKey(r))
	if errors.Is(err, pebble.ErrNotFound) {
		return "", nil
	}

	if err != nil {
		return "", errors.Wrap(err, "read backfill cursor")
	}

	defer func() { _ = closer.Close() }()

	var state backfillState
	if err := json.Unmarshal(data, &state); err != nil {
		// Unreadable is indistinguishable from absent, and both mean the same
		// thing: nothing here can be trusted to say what has been copied.
		return "", nil
	}

	// The start needs no check: it is the key this record was found under, so a
	// cursor read here was written by a range beginning in the same place. With
	// the end and the owner matching too, it is the same range on the same node,
	// and the position inside it still means what it meant.
	if state.End != r.End || state.Owner != r.Owner {
		return "", nil
	}

	return state.At, nil
}

// setBackfillCursor records how far a backfill has got.
//
// Synced, for the same reason the entries it describes are: the cursor and the
// data must survive a crash together or the pair is worse than neither. An
// unsynced cursor that outlived its entries would claim keys that are not here.
func (s *Shard) setBackfillCursor(r rangemap.Range, at string) error {
	data, err := json.Marshal(backfillState{End: r.End, Owner: r.Owner, At: at})
	if err != nil {
		return errors.Wrap(err, "marshal backfill cursor")
	}

	if err := s.db.Set(backfillKey(r), data, pebble.Sync); err != nil {
		return errors.Wrap(err, "record backfill cursor")
	}

	return nil
}

// clearBackfillCursor forgets a range's backfill position.
func (s *Shard) clearBackfillCursor(r rangemap.Range) error {
	if err := s.db.Delete(backfillKey(r), pebble.Sync); err != nil {
		return errors.Wrap(err, "clear backfill cursor")
	}

	return nil
}

// backfillKey is where a range's cursor lives: 'm' + 'b' + the range's start.
//
// Under the meta prefix rather than among the entries, so a backfill's own
// bookkeeping is never something a backfill copies — the object walk is bounded
// by the object prefix, and a cursor sorting inside it would be shipped to the
// next learner as an entry that describes nothing.
func backfillKey(r rangemap.Range) []byte {
	out := make([]byte, 0, 2+len(r.Start))
	out = append(out, keyspace.Meta, backfillMeta)

	return append(out, r.Start...)
}

// backfillMeta distinguishes cursors from whatever else the meta prefix comes
// to hold.
const backfillMeta = 'b'

// backfillPrefix is every cursor this shard has recorded.
var backfillPrefix = []byte{keyspace.Meta, backfillMeta}

// dropBackfillCursors forgets every recorded position.
//
// Called where the entries a cursor describes are removed wholesale. A cursor
// that outlived its data is the one failure mode of this design: it claims a
// prefix is present, the prefix is gone, and a resumed backfill skips it.
func (s *Shard) dropBackfillCursors() error {
	upper := keyspace.UpperBound(backfillPrefix)
	if err := s.db.DeleteRange(backfillPrefix, upper, pebble.Sync); err != nil {
		return errors.Wrap(err, "drop backfill cursors")
	}

	return nil
}
