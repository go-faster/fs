package clusterstore

import (
	"context"
	"sync/atomic"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// scanFetch is how many entries one query asks for.
//
// Larger than a page, for the same reason indexFetch is: folding collapses many
// keys into one entry, so a listing with a delimiter over a deep prefix would
// otherwise refill after every fold. It is not larger than that, because the
// buffer is held for the length of the page.
const scanFetch = 256

// scanSource is the cluster-scope production of a listing: one store holds
// every object, so a page is a range scan rather than a merge.
//
// What disappears here is worth naming, because it is the whole point of
// cluster scope. There is no fan-out, so a page costs one query instead of N
// RPCs whose p99 is the slowest of N. There is no replica resolution, because
// the store holds one row per object rather than one per copy — the newest
// record was chosen when it was written, by the same ordering rule, instead of
// on every read. And there is no per-node readiness to check, because there is
// one store and it is either usable or it is not.
//
// What survives is the availability trade the store makes in exchange: a node
// that cannot be reached used to cost only the objects it held, and now an
// unreachable store costs the listing. That is why the sidecar-walk fallback
// stays wired.
type scanSource struct {
	store  metastore.Store
	bucket string
	prefix string

	// after is the exclusive cursor the next query resumes from. It moves as
	// entries are consumed and jumps ahead when a group is skipped.
	after string

	// queries counts the store queries this listing cost, for the metric that
	// makes "one query per page" observable rather than merely asserted.
	queries *atomic.Int64

	buf []transport.IndexEntry
	pos int
	// drained reports that the store has nothing after what is buffered.
	drained bool
}

var _ entrySource = (*scanSource)(nil)

// newScanSource opens a cluster-scope listing, refusing it if the store is not
// ready.
//
// Refusing rather than answering short is the same rule the merge applies to a
// node still building: a listing missing keys is a wrong answer, where the
// slower sidecar walk is a right one.
func newScanSource(
	ctx context.Context,
	store metastore.Store,
	bucket, prefix, after string,
	queries *atomic.Int64,
) (*scanSource, error) {
	state, err := store.State(ctx)
	if err != nil {
		// An unreachable store degrades the listing; it does not fail it. The
		// caller walks the sidecars, which is slower and always right.
		return nil, ErrIndexUnavailable
	}

	if state != metastore.StateReady {
		return nil, ErrIndexUnavailable
	}

	return &scanSource{
		store:   store,
		bucket:  bucket,
		prefix:  prefix,
		after:   after,
		queries: queries,
	}, nil
}

// refill runs one query when the buffer is empty.
//
// This is the query the whole change exists to make singular: one page of a
// listing issues exactly one of these, and nothing above it fans out.
func (s *scanSource) refill(ctx context.Context) error {
	if s.pos < len(s.buf) || s.drained {
		return nil
	}

	s.buf = s.buf[:0]
	s.pos = 0

	s.queries.Add(1)

	err := s.store.Scan(ctx, s.bucket, s.prefix, s.after, scanFetch,
		func(e metastore.Entry) error {
			s.buf = append(s.buf, transport.IndexEntry{
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
		// A store that stops answering mid-listing is the case the fallback
		// exists for. Unlike a node dropping out of a merge, there is nothing
		// else holding these keys, so the page cannot simply continue.
		return ErrIndexUnavailable
	}

	s.drained = len(s.buf) < scanFetch

	return nil
}

// next implements entrySource.
func (s *scanSource) next(ctx context.Context) (transport.IndexEntry, bool, error) {
	if err := s.refill(ctx); err != nil {
		return transport.IndexEntry{}, false, err
	}

	if s.pos >= len(s.buf) {
		return transport.IndexEntry{}, false, nil
	}

	entry := s.buf[s.pos]
	s.pos++
	s.after = entry.Key

	return entry, true, nil
}

// peek implements entrySource.
func (s *scanSource) peek(ctx context.Context) (bool, error) {
	if err := s.refill(ctx); err != nil {
		return false, err
	}

	return s.pos < len(s.buf), nil
}

// skipGroup implements entrySource, moving past every key under a folded
// prefix.
//
// Buffered keys inside the group are dropped, and the cursor jumps to the
// prefix followed by 0xFF — a byte no UTF-8 key contains, so it sorts after
// everything under the prefix while still sorting before the first key that is
// not, which may itself be a real key that must not be skipped.
func (s *scanSource) skipGroup(prefix string) {
	next := prefixSuccessor(prefix)
	if next == "" {
		// A prefix of all 0xFF has no successor; nothing sorts after it.
		return
	}

	for s.pos < len(s.buf) && s.buf[s.pos].Key < next {
		s.pos++
	}

	if bound := prefix + "\xff"; s.after < bound {
		s.after = bound

		// The buffer was filled from before the jump, so whatever remains in it
		// is inside the group or behind the new cursor. Dropping it forces the
		// next read to query from the cursor — which is the seek that makes a
		// folded group cost one query rather than one read per key in it.
		s.buf = s.buf[:0]
		s.pos = 0
		s.drained = false
	}
}
