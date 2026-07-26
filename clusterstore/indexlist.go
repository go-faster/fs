package clusterstore

import (
	"context"
	"strings"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// indexPeer is the optional half of a Peer that can answer from a node's own
// object index. A peer without it — an older binary, or a store wired without
// an index — makes the listing fall back to reading sidecars.
type indexPeer interface {
	IndexPage(ctx context.Context, q transport.IndexQuery) (transport.IndexPage, error)
}

// ErrIndexUnavailable reports that a listing cannot be served from the object
// indexes, and the caller should read the sidecars instead.
//
// It is not a failure. A node still building its index would simply be missing
// from the merge, and a listing short of keys is a wrong answer, where the
// slower walk is a right one.
var ErrIndexUnavailable = errors.New("object index unavailable")

// indexFetch is how many entries one node is asked for at a time. It is larger
// than a typical page because folding collapses many keys into one entry: a
// listing with a delimiter over a deep prefix would otherwise refill after
// every fold.
const indexFetch = 256

// ListPage returns one page of a bucket's objects, folded by delimiter, served
// from the nodes' object indexes.
//
// Each node answers from its own index in key order, and the pages are merged
// here. A page therefore costs what the page contains — a bounded number of
// entries from each node — instead of a scan of every disk and a read of every
// sidecar, which is what the listing did before and still does when this path
// is unavailable.
//
// Every object is indexed by each node holding a copy of it, so the merge sees
// a key once per replica and keeps the newest by the same total order the
// sidecars use. A node that cannot be reached is skipped: its objects are
// indexed on the nodes holding the other copies, which is the same availability
// bound reads and the sidecar walk already have. A node whose index is not
// ready is different — its objects may exist nowhere else in the merge — so
// that returns ErrIndexUnavailable and the caller walks instead.
func (c *Coordinator) ListPage(
	ctx context.Context,
	bucket, prefix, delimiter, after string,
	limit int,
) (objects []*Sidecar, commonPrefixes []string, truncated bool, err error) {
	topo := c.topo.Topology()

	streams, err := c.openIndexStreams(ctx, topo, bucket, prefix, after)
	if err != nil {
		return nil, nil, false, err
	}

	var (
		prefixes []string
		last     string
		more     bool
	)

	for {
		if limit > 0 && len(objects)+len(prefixes) >= limit {
			// One more entry decides truncation: a page that ends exactly on
			// the last key is not truncated, and claiming otherwise costs the
			// caller a wasted request.
			next, err := peekStreams(ctx, streams)
			if err != nil {
				return nil, nil, false, err
			}

			more = next

			break
		}

		entry, ok, err := nextEntry(ctx, streams)
		if err != nil {
			return nil, nil, false, err
		}

		if !ok {
			break
		}

		folded, isPrefix := foldKey(entry.Key, prefix, delimiter)
		if isPrefix {
			// A cursor naming a common prefix — or falling inside one — means
			// that whole group was already returned. Its keys still sort after
			// the cursor, so they arrive here and fold straight back into an
			// entry the client has seen; without this they would be served
			// again on every subsequent page.
			if folded <= after {
				skipGroup(streams, folded)

				continue
			}

			if folded == last {
				continue
			}

			prefixes = append(prefixes, folded)
			last = folded

			// Skip the rest of the group in one step rather than reading it:
			// every key under a folded prefix produces the same entry, and on
			// a deep prefix that is the difference between one seek and a
			// million reads.
			skipGroup(streams, folded)

			continue
		}

		objects = append(objects, &Sidecar{
			Bucket:     bucket,
			Key:        entry.Key,
			Size:       entry.Size,
			ETag:       entry.ETag,
			Modified:   entry.Modified,
			Seq:        entry.Seq,
			Generation: entry.Generation,
			Owner:      fs.Owner{ID: entry.OwnerID, DisplayName: entry.OwnerName},
		})
		last = entry.Key
	}

	return objects, prefixes, more, nil
}

// foldKey applies the delimiter, returning the common prefix a key folds into
// or the key itself.
func foldKey(key, prefix, delimiter string) (string, bool) {
	if delimiter == "" {
		return key, false
	}

	rest := strings.TrimPrefix(key, prefix)

	idx := strings.Index(rest, delimiter)
	if idx < 0 {
		return key, false
	}

	return prefix + rest[:idx+len(delimiter)], true
}

// indexStream is one node's page of the merge.
type indexStream struct {
	peer    indexPeer
	query   transport.IndexQuery
	entries []transport.IndexEntry
	pos     int
	// drained reports that the node has no more entries after what is buffered.
	drained bool
}

// openIndexStreams asks every node for its first page, refusing the whole
// listing if any node's index is not ready.
func (c *Coordinator) openIndexStreams(ctx context.Context, topo *cluster.Topology, bucket, prefix, after string) ([]*indexStream, error) {
	type result struct {
		stream *indexStream
		err    error
	}

	results := make([]result, len(topo.Nodes))

	var wg sync.WaitGroup

	for i := range topo.Nodes {
		node := topo.Nodes[i]

		wg.Go(func() {
			p, err := c.peers.Peer(node)
			if err != nil {
				// Unreachable: its objects are indexed by the nodes holding
				// the other copies.
				return
			}

			ip, ok := p.(indexPeer)
			if !ok {
				results[i] = result{err: ErrIndexUnavailable}

				return
			}

			stream := &indexStream{
				peer: ip,
				query: transport.IndexQuery{
					Bucket: bucket,
					Prefix: prefix,
					After:  after,
					Limit:  indexFetch,
				},
			}

			page, err := ip.IndexPage(ctx, stream.query)
			if err != nil {
				if errors.Is(err, transport.ErrUnsupported) {
					results[i] = result{err: ErrIndexUnavailable}

					return
				}

				// A node that failed to answer is treated as unreachable.
				return
			}

			if !page.Ready {
				results[i] = result{err: ErrIndexUnavailable}

				return
			}

			stream.entries = page.Entries
			stream.drained = len(page.Entries) < stream.query.Limit
			results[i] = result{stream: stream}
		})
	}

	wg.Wait()

	streams := make([]*indexStream, 0, len(results))

	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}

		if r.stream != nil {
			streams = append(streams, r.stream)
		}
	}

	if len(streams) == 0 {
		return nil, ErrIndexUnavailable
	}

	return streams, nil
}

// refill tops a stream up when its buffer runs out.
func (s *indexStream) refill(ctx context.Context) error {
	if s.pos < len(s.entries) || s.drained {
		return nil
	}

	page, err := s.peer.IndexPage(ctx, s.query)
	if err != nil {
		// A node that stops answering mid-listing is treated as unreachable
		// from here on, the same as one that never answered.
		s.drained = true
		s.entries = nil
		s.pos = 0

		return nil //nolint:nilerr // Degrading to the other replicas is the contract.
	}

	if !page.Ready {
		return ErrIndexUnavailable
	}

	s.entries = page.Entries
	s.pos = 0
	s.drained = len(page.Entries) < s.query.Limit

	return nil
}

// head returns the stream's next entry without consuming it.
func (s *indexStream) head(ctx context.Context) (transport.IndexEntry, bool, error) {
	if err := s.refill(ctx); err != nil {
		return transport.IndexEntry{}, false, err
	}

	if s.pos >= len(s.entries) {
		return transport.IndexEntry{}, false, nil
	}

	return s.entries[s.pos], true, nil
}

// advance consumes the head and moves the stream's cursor past it, so a refill
// resumes after what has been read.
func (s *indexStream) advance() {
	if s.pos < len(s.entries) {
		s.query.After = s.entries[s.pos].Key
		s.pos++
	}
}

// seekPast moves a stream past every key under a folded prefix, so the group
// costs one seek instead of one read per key in it.
func (s *indexStream) seekPast(prefix string) {
	next := prefixSuccessor(prefix)
	if next == "" {
		// A prefix of all 0xFF has no successor. Nothing sorts after it, so
		// there is nothing to skip to.
		return
	}

	// Anything already buffered from inside the group is dropped here.
	for s.pos < len(s.entries) && s.entries[s.pos].Key < next {
		s.pos++
	}

	// And a refill resumes past it. The bound is the prefix followed by 0xFF,
	// which no object key can contain — keys are UTF-8 — so it sorts after
	// every key under the prefix while still sorting before the first key that
	// is not, which may itself be a real key that must not be skipped.
	if bound := prefix + "\xff"; s.query.After < bound {
		s.query.After = bound
	}
}

// nextEntry returns the next key in merged order, resolving replicas of the
// same key to the newest record.
func nextEntry(ctx context.Context, streams []*indexStream) (transport.IndexEntry, bool, error) {
	var (
		best  transport.IndexEntry
		found bool
	)

	for _, s := range streams {
		entry, ok, err := s.head(ctx)
		if err != nil {
			return transport.IndexEntry{}, false, err
		}

		if !ok {
			continue
		}

		if !found || entry.Key < best.Key {
			best, found = entry, true
		}
	}

	if !found {
		return transport.IndexEntry{}, false, nil
	}

	// Every stream holding this key contributes a candidate; the newest wins by
	// the sidecars' own order, so a node that has not caught up with an
	// overwrite cannot serve a stale size into the listing.
	for _, s := range streams {
		entry, ok, err := s.head(ctx)
		if err != nil {
			return transport.IndexEntry{}, false, err
		}

		if !ok || entry.Key != best.Key {
			continue
		}

		if newerEntry(entry, best) {
			best = entry
		}

		s.advance()
	}

	return best, true, nil
}

// peekStreams reports whether any entry remains, without consuming it.
func peekStreams(ctx context.Context, streams []*indexStream) (bool, error) {
	for _, s := range streams {
		_, ok, err := s.head(ctx)
		if err != nil {
			return false, err
		}

		if ok {
			return true, nil
		}
	}

	return false, nil
}

// skipGroup moves every stream past a folded prefix.
func skipGroup(streams []*indexStream, prefix string) {
	for _, s := range streams {
		s.seekPast(prefix)
	}
}

// newerEntry applies the sidecars' total order: sequence, then write time, then
// the generation stamp.
func newerEntry(a, b transport.IndexEntry) bool {
	if a.Seq != b.Seq {
		return a.Seq > b.Seq
	}

	if !a.Modified.Equal(b.Modified) {
		return a.Modified.After(b.Modified)
	}

	return a.Generation > b.Generation
}

// prefixSuccessor is the smallest string greater than every string starting
// with prefix: the prefix with its last usable byte incremented. It returns
// empty when none exists, which happens only for a prefix of all 0xFF — bytes
// no UTF-8 key contains.
func prefixSuccessor(prefix string) string {
	out := []byte(prefix)

	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xFF {
			out[i]++

			return string(out[:i+1])
		}
	}

	return ""
}
