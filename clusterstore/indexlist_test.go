package clusterstore

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/transport"
	"github.com/go-faster/fs/storagetest"
)

// testIndex answers index queries for one node out of the sidecars that node
// actually stores — the same relationship a real index has to the disks it
// watches, without a pebble store in the test. Reading them on every query is
// exactly what the index exists to avoid, which is fine here: this stands in
// for the index so the merge above it can be tested against real writes.
type testIndex struct {
	fc    *fakeCluster
	node  cluster.NodeID
	ready *atomic.Bool
	calls *atomic.Int64
}

func (t testIndex) page(_ context.Context, q transport.IndexQuery) (transport.IndexPage, error) {
	t.calls.Add(1)

	if !t.ready.Load() {
		return transport.IndexPage{}, nil
	}

	store := t.fc.stores[t.node]

	var entries []transport.IndexEntry

	seen := make(map[string]struct{})

	for _, disk := range t.fc.topo.Nodes[0].Disks {
		names, err := store.List(context.Background(), disk.ID, "obj/")
		if err != nil {
			continue
		}

		for _, name := range names {
			if !strings.HasSuffix(name, "/meta") {
				continue
			}

			sc, err := readSidecarFrom(context.Background(), LocalPeer{Store: store}, disk.ID, name)
			if err != nil || sc == nil || sc.Bucket != q.Bucket {
				continue
			}

			if _, dup := seen[sc.Key]; dup {
				continue
			}

			seen[sc.Key] = struct{}{}

			if !strings.HasPrefix(sc.Key, q.Prefix) || sc.Key <= q.After {
				continue
			}

			entries = append(entries, transport.IndexEntry{
				Key:        sc.Key,
				Size:       sc.Size,
				ETag:       sc.ETag,
				Modified:   sc.Modified,
				Seq:        sc.Seq,
				Generation: sc.Generation,
				OwnerID:    sc.Owner.ID,
				OwnerName:  sc.Owner.DisplayName,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	if q.Limit > 0 && len(entries) > q.Limit {
		entries = entries[:q.Limit]
	}

	return transport.IndexPage{Ready: true, Entries: entries}, nil
}

// indexState is the per-node index switchboard a test drives.
type indexState struct {
	mu    sync.Mutex
	ready map[cluster.NodeID]*atomic.Bool
	calls *atomic.Int64
}

func newIndexState(fc *fakeCluster) *indexState {
	st := &indexState{ready: make(map[cluster.NodeID]*atomic.Bool), calls: new(atomic.Int64)}

	for _, n := range fc.topo.Nodes {
		ready := new(atomic.Bool)
		ready.Store(true)
		st.ready[n.ID] = ready
	}

	return st
}

func (s *indexState) set(id cluster.NodeID, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ready[id].Store(ready)
}

// indexedPeers dials peers whose local index is backed by testIndex.
type indexedPeers struct {
	fc    *fakeCluster
	state *indexState
}

func (p indexedPeers) Peer(node cluster.Node) (Peer, error) {
	base, err := p.fc.Peer(node)
	if err != nil {
		return nil, err
	}

	local, ok := base.(LocalPeer)
	if !ok {
		// A node the fake cluster is holding down. A real one is still dialable
		// and simply fails the request, so that is what this models — a peer
		// that cannot be asked at all is an older binary, which is a different
		// case with a different answer.
		return downIndexPeer{Peer: base}, nil
	}

	local.Index = testIndex{fc: p.fc, node: node.ID, ready: p.state.ready[node.ID], calls: p.state.calls}.page

	return local, nil
}

// downIndexPeer is a reachable peer whose index request fails.
type downIndexPeer struct{ Peer }

func (downIndexPeer) IndexPage(context.Context, transport.IndexQuery) (transport.IndexPage, error) {
	return transport.IndexPage{}, errPeerDown
}

var errPeerDown = errors.New("peer down")

// indexedCoordinator builds a coordinator whose peers can answer from an index.
func indexedCoordinator(t *testing.T, fc *fakeCluster) (*Coordinator, *indexState) {
	t.Helper()

	state := newIndexState(fc)

	c, err := New(Config{
		Topology: fakeTopoSource{fc: fc},
		Peers:    indexedPeers{fc: fc, state: state},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c, state
}

// listBoth returns the same page from the index path and from the sidecar
// walk. Every case below asserts they agree: the index is only worth having if
// it answers what the walk would have.
func listBoth(t *testing.T, c *Coordinator, req *fs.ListObjectsRequest) (indexed, walked *fs.ListObjectsResponse) {
	t.Helper()

	s := NewStorage(c)

	indexed, err := s.listFromIndex(t.Context(), req)
	require.NoError(t, err, "the index path must be available in this test")

	walked, err = s.listFromSidecars(t.Context(), req)
	require.NoError(t, err)

	return indexed, walked
}

func TestListPageMatchesTheWalk(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, _ := indexedCoordinator(t, fc)

	keys := []string{
		"a.txt", "b.txt",
		"docs/one.txt", "docs/two.txt", "docs/deep/three.txt",
		"images/x.png",
		"z.txt",
	}

	for _, key := range keys {
		mustPut(t, c, key, randBytes(64))
	}

	c.Flush()

	for _, tt := range []struct {
		name string
		req  fs.ListObjectsRequest
	}{
		{"everything", fs.ListObjectsRequest{Bucket: "b"}},
		{"limit", fs.ListObjectsRequest{Bucket: "b", Limit: 3}},
		{"prefix", fs.ListObjectsRequest{Bucket: "b", Prefix: "docs/"}},
		{"delimiter", fs.ListObjectsRequest{Bucket: "b", Delimiter: "/"}},
		{"delimiter and prefix", fs.ListObjectsRequest{Bucket: "b", Prefix: "docs/", Delimiter: "/"}},
		{"delimiter and limit", fs.ListObjectsRequest{Bucket: "b", Delimiter: "/", Limit: 2}},
		{"after a key", fs.ListObjectsRequest{Bucket: "b", StartAfter: "b.txt"}},
		{"after a common prefix", fs.ListObjectsRequest{Bucket: "b", Delimiter: "/", StartAfter: "docs/"}},
		{"after everything", fs.ListObjectsRequest{Bucket: "b", StartAfter: "zzz"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			indexed, walked := listBoth(t, c, &req)

			assert.Equal(t, keysOf(walked), keysOf(indexed), "objects")
			assert.Equal(t, walked.CommonPrefixes, indexed.CommonPrefixes, "common prefixes")
			assert.Equal(t, walked.IsTruncated, indexed.IsTruncated, "truncation")
			assert.Equal(t, walked.NextStartAfter, indexed.NextStartAfter, "resume point")
		})
	}
}

// TestListPagePagesTheWholeBucket walks a bucket a page at a time through the
// index and asserts it sees every key exactly once, in order — the property a
// client crawling with a continuation token depends on.
func TestListPagePagesTheWholeBucket(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c, _ := indexedCoordinator(t, fc)

	var want []string

	for i := range 25 {
		key := "k" + string(rune('a'+i%25)) + "/obj"
		want = append(want, key)
		mustPut(t, c, key, randBytes(16))
	}

	c.Flush()
	sort.Strings(want)

	s := NewStorage(c)

	var (
		got   []string
		after string
	)

	for {
		page, err := s.listFromIndex(t.Context(), &fs.ListObjectsRequest{
			Bucket: "b", StartAfter: after, Limit: 4,
		})
		require.NoError(t, err)

		for _, o := range page.Objects {
			got = append(got, o.Key)
		}

		if !page.IsTruncated {
			break
		}

		require.NotEmpty(t, page.NextStartAfter)
		after = page.NextStartAfter
	}

	assert.Equal(t, want, got)
}

// TestListPageMergesReplicas checks the merge treats copies of one object as
// one entry, and keeps the newest — a node that has not caught up with an
// overwrite must not serve a stale size into the listing.
func TestListPageMergesReplicas(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c, _ := indexedCoordinator(t, fc)

	mustPut(t, c, "a.txt", randBytes(500))
	c.Flush()

	mustPut(t, c, "a.txt", randBytes(20))
	c.Flush()

	page, err := NewStorage(c).listFromIndex(t.Context(), &fs.ListObjectsRequest{Bucket: "b"})
	require.NoError(t, err)

	require.Len(t, page.Objects, 1, "one object, however many nodes hold it")
	assert.Equal(t, int64(20), page.Objects[0].Size, "the newest record wins")
}

// TestListPageSkipsFoldedGroups is the reason folding lives below the page: a
// common prefix must cost a seek, not a read of every key beneath it.
func TestListPageSkipsFoldedGroups(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c, state := indexedCoordinator(t, fc)

	// One shallow key and a deep prefix holding many.
	mustPut(t, c, "a.txt", randBytes(16))

	for i := range 40 {
		mustPut(t, c, "deep/"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt", randBytes(16))
	}

	c.Flush()

	before := state.calls.Load()

	page, err := NewStorage(c).listFromIndex(t.Context(), &fs.ListObjectsRequest{
		Bucket: "b", Delimiter: "/",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"deep/"}, page.CommonPrefixes)
	require.Len(t, page.Objects, 1)

	fetches := state.calls.Load() - before
	assert.LessOrEqual(t, fetches, int64(2*len(fc.topo.Nodes)),
		"a folded group costs a seek per node, not a page per key in it")
}

// TestListPageFallsBackWhenAnIndexIsNotReady: a node still building its index
// would simply be missing from the merge, and a listing short of keys is a
// wrong answer. The caller is told to walk instead.
func TestListPageFallsBackWhenAnIndexIsNotReady(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c, state := indexedCoordinator(t, fc)

	mustPut(t, c, "a.txt", randBytes(16))
	c.Flush()

	state.set(fc.topo.Nodes[1].ID, false)

	_, err := NewStorage(c).listFromIndex(t.Context(), &fs.ListObjectsRequest{Bucket: "b"})
	require.ErrorIs(t, err, ErrIndexUnavailable)

	// And the public entry point serves the page anyway, from the sidecars.
	require.NoError(t, NewStorage(c).coord.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	page, err := NewStorage(c).ListObjects(t.Context(), &fs.ListObjectsRequest{Bucket: "b"})
	require.NoError(t, err)
	require.Len(t, page.Objects, 1)
	assert.Equal(t, "a.txt", page.Objects[0].Key)
}

// TestListPageToleratesAnUnreachableNode: an object is indexed by every node
// holding a copy, so losing one node loses nothing from the listing — the same
// availability bound reads already have.
func TestListPageToleratesAnUnreachableNode(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c, _ := indexedCoordinator(t, fc)

	for _, key := range []string{"a.txt", "b.txt", "c.txt"} {
		mustPut(t, c, key, randBytes(16))
	}

	c.Flush()

	fc.setDown(fc.topo.Nodes[2].ID, true)

	page, err := NewStorage(c).listFromIndex(t.Context(), &fs.ListObjectsRequest{Bucket: "b"})
	require.NoError(t, err)

	assert.Equal(t, []string{"a.txt", "b.txt", "c.txt"}, keysOf(page),
		"every object still has a reachable holder")
}

// keysOf is the listing's keys, for comparing two answers.
func keysOf(res *fs.ListObjectsResponse) []string {
	out := make([]string, 0, len(res.Objects))
	for _, o := range res.Objects {
		out = append(out, o.Key)
	}

	return out
}

// TestConformanceThroughTheIndex runs the whole fs.Storage conformance suite
// against a cluster whose listings are served from the object indexes.
//
// It is the strongest statement available that the index path answers what the
// sidecar walk answers: the suite covers listing order, prefixes, delimiters,
// nested keys, deletes and the empty-bucket edges, and it is the same suite the
// single-node backends must pass. A listing that quietly lost, duplicated or
// misordered a key fails here rather than in a client.
func TestConformanceThroughTheIndex(t *testing.T) {
	storagetest.Run(t, func(tb testing.TB) fs.Storage {
		fc := newFakeCluster(3, 2)
		state := newIndexState(fc)

		c, err := New(Config{
			Topology: fakeTopoSource{fc: fc},
			Peers:    indexedPeers{fc: fc, state: state},
		})
		require.NoError(tb, err)
		tb.Cleanup(func() { _ = c.Close() })

		return NewStorage(c)
	})
}

// TestListObjectsPrefersTheIndex guards against the quietest possible failure:
// the whole feature falling back to the walk on every request, which every
// other test would still pass.
func TestListObjectsPrefersTheIndex(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c, state := indexedCoordinator(t, fc)

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))
	mustPut(t, c, "a.txt", randBytes(16))
	c.Flush()

	before := state.calls.Load()

	page, err := NewStorage(c).ListObjects(t.Context(), &fs.ListObjectsRequest{Bucket: "b"})
	require.NoError(t, err)
	require.Len(t, page.Objects, 1)

	assert.Greater(t, state.calls.Load(), before, "the listing was served from the index")
}
