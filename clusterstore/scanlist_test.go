package clusterstore

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/memstore"
	"github.com/go-faster/fs/internal/cluster/objindex"
	"github.com/go-faster/fs/storagetest"
)

// clusterCoordinator builds a coordinator backed by a cluster-scope store
// instead of by per-node indexes.
//
// The store is fed by the write path, through the same observer seam the real
// one uses: every record that lands on a node's disks is reported and indexed.
// Filling it afterwards would test the listing against a store that agrees with
// the disks by construction, which is not the thing worth checking.
func clusterCoordinator(t *testing.T, fc *fakeCluster) (*Coordinator, metastore.Store) {
	t.Helper()

	store := memstore.New()
	require.NoError(t, store.MarkReady(t.Context()))

	for _, node := range fc.topo.Nodes {
		fc.stores[node.ID].observe(indexRecord(t, store), forgetRecord(t, store))
	}

	c, err := New(Config{
		Topology:  fakeTopoSource{fc: fc},
		Peers:     indexedPeers{fc: fc, state: newIndexState(fc)},
		Metastore: store,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	// A real bucket record, because a rebuild discovers buckets from them and
	// the storage layer refuses a listing without one.
	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	return c, store
}

// indexRecord returns a commit observer that indexes object records, and
// forgetRecord one that removes them — together, what cmd/fs's objectIndexer
// does against the node-local index.
func indexRecord(t testing.TB, store metastore.Store) func(cluster.DiskID, string, []byte) {
	t.Helper()

	return func(_ cluster.DiskID, name string, data []byte) {
		sc, ok := objectRecord(name, data)
		if !ok {
			return
		}

		_ = store.Put(context.Background(), metastore.Entry{
			Bucket:     sc.Bucket,
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

func forgetRecord(t testing.TB, store metastore.Store) func(cluster.DiskID, string, []byte) {
	t.Helper()

	return func(_ cluster.DiskID, name string, data []byte) {
		if sc, ok := objectRecord(name, data); ok {
			_ = store.Delete(context.Background(), sc.Bucket, sc.Key)
		}
	}
}

// objectRecord decodes a stored record when it is an object's commit record.
//
// Both halves of the name matter: bucket records end in "/meta" too, and
// feeding one to an index that only understands objects is how a bucket ends up
// listed as an object.
func objectRecord(name string, data []byte) (*Sidecar, bool) {
	if !strings.HasPrefix(name, "obj/") || !strings.HasSuffix(name, "/meta") {
		return nil, false
	}

	sc, err := decodeSidecar(data)
	if err != nil || sc == nil || sc.Bucket == "" || sc.Key == "" {
		return nil, false
	}

	return sc, true
}

// TestConformanceThroughClusterScope runs the whole fs.Storage suite against a
// cluster whose listings come from a cluster-scope store.
//
// #148 asks for the suite on **both** scopes — "the matrix, not one arm" — and
// this is the other arm of TestConformanceThroughTheIndex. It is the strongest
// statement available that scope is an implementation detail: every case an S3
// client's behavior is pinned by passes identically whether a page came from a
// merge across nodes or from one scan.
func TestConformanceThroughClusterScope(t *testing.T) {
	storagetest.Run(t, func(tb testing.TB) fs.Storage {
		fc := newFakeCluster(3, 2)

		store := memstore.New()
		if err := store.MarkReady(context.Background()); err != nil {
			tb.Fatal(err)
		}

		for _, node := range fc.topo.Nodes {
			fc.stores[node.ID].observe(indexRecord(tb, store), forgetRecord(tb, store))
		}

		c, err := New(Config{
			Topology:  fakeTopoSource{fc: fc},
			Peers:     indexedPeers{fc: fc, state: newIndexState(fc)},
			Metastore: store,
		})
		require.NoError(tb, err)
		tb.Cleanup(func() { _ = c.Close() })

		return NewStorage(c)
	})
}

// TestClusterScopeListingMatchesTheWalk is the case that matters: the same
// table the merge path is held to, against a cluster-scope store.
//
// A listing that answered differently depending on which scope produced it
// would make the whole plan unshippable — the scope is meant to be an
// implementation detail of how entries are found, not a change in what an S3
// client sees.
func TestClusterScopeListingMatchesTheWalk(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, _ := clusterCoordinator(t, fc)

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
			scanned, walked := listBoth(t, c, &req)

			assert.Equal(t, keysOf(walked), keysOf(scanned), "objects")
			assert.Equal(t, walked.CommonPrefixes, scanned.CommonPrefixes, "common prefixes")
			assert.Equal(t, walked.IsTruncated, scanned.IsTruncated, "truncation")
			assert.Equal(t, walked.NextStartAfter, scanned.NextStartAfter, "resume point")
		})
	}
}

// TestClusterScopePagesTheWholeBucket: a client crawling with a continuation
// token must see every key exactly once, in order, across pages that each cost
// one query.
func TestClusterScopePagesTheWholeBucket(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c, _ := clusterCoordinator(t, fc)

	var want []string

	for _, key := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		mustPut(t, c, key, randBytes(16))

		want = append(want, key)
	}

	c.Flush()

	var (
		got   []string
		after string
	)

	for range len(want) + 2 {
		objects, _, more, err := c.ListPage(t.Context(), "b", "", "", after, 2)
		require.NoError(t, err)

		for _, sc := range objects {
			got = append(got, sc.Key)
		}

		if !more {
			break
		}

		require.NotEmpty(t, objects, "a truncated page must carry a resume point")
		after = objects[len(objects)-1].Key
	}

	assert.Equal(t, want, got)
}

// TestClusterScopeIssuesOneQueryPerPage is the acceptance criterion, measured
// through the listing rather than at the store: a page of a plain listing costs
// exactly one Scan, where the merge path costs one RPC per node.
func TestClusterScopeIssuesOneQueryPerPage(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, store := clusterCoordinator(t, fc)

	for _, key := range []string{"a", "b", "c", "d"} {
		mustPut(t, c, key, randBytes(16))
	}

	c.Flush()

	counted := &countingStore{Store: store}
	c.meta = counted

	objects, _, _, err := c.ListPage(t.Context(), "b", "", "", "", 2)
	require.NoError(t, err)
	require.Len(t, objects, 2)

	assert.Equal(t, 1, counted.scans, "a listing page must cost one scan, whatever the cluster size")
}

// countingStore counts the scans a listing issues.
type countingStore struct {
	metastore.Store

	scans int
}

func (s *countingStore) Scan(
	ctx context.Context,
	bucket, prefix, after string,
	limit int,
	fn func(metastore.Entry) error,
) error {
	s.scans++

	return s.Store.Scan(ctx, bucket, prefix, after, limit, fn)
}

// TestClusterScopeFallsBackWhenTheStoreIsNotReady: a store still building holds
// an unknown fraction of the cluster, so a listing from it would be short — and
// a listing missing keys is a wrong answer where the slower walk is a right one.
func TestClusterScopeFallsBackWhenTheStoreIsNotReady(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, store := clusterCoordinator(t, fc)

	mustPut(t, c, "a.txt", randBytes(16))
	c.Flush()

	require.NoError(t, store.MarkBuilding(t.Context()))

	objects, _, _, err := c.ListPage(t.Context(), "b", "", "", "", 0)
	require.ErrorIs(t, err, ErrIndexUnavailable)
	require.Empty(t, objects)

	// And the storage layer turns that into the walk rather than an error,
	// which is the behavior an S3 client actually sees.
	res, err := NewStorage(c).ListObjects(t.Context(), &fs.ListObjectsRequest{Bucket: "b"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a.txt"}, keysOf(res))
}

// TestClusterScopeUsageIsReadNotWalked: at cluster scope the counters describe
// the whole cluster already, so a bucket's totals are a read. The merge path
// pages the entire bucket to reach the same number.
func TestClusterScopeUsageIsReadNotWalked(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c, store := clusterCoordinator(t, fc)

	for _, key := range []string{"a", "b", "c"} {
		mustPut(t, c, key, randBytes(32))
	}

	c.Flush()

	counted := &countingStore{Store: store}
	c.meta = counted

	totals, err := c.countBucketIndexed(t.Context(), "b")
	require.NoError(t, err)

	assert.Equal(t, int64(3), totals.Objects)
	assert.Equal(t, int64(96), totals.Bytes)
	assert.Zero(t, counted.scans, "usage at cluster scope must not scan the bucket")
}

// TestLocalScopeStoreChangesNothing: a store that describes one node is not the
// cluster, so wiring one in must leave the merge exactly as it was. This is
// what makes the Metastore field safe to set unconditionally.
func TestLocalScopeStoreChangesNothing(t *testing.T) {
	fc := newFakeCluster(3, 2)

	idx, err := objindex.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	require.NoError(t, idx.MarkReady(t.Context()))

	c, err := New(Config{
		Topology: fakeTopoSource{fc: fc},
		Peers:    indexedPeers{fc: fc, state: newIndexState(fc)},
		// Deliberately not wrapped: objindex reports ScopeLocal.
		Metastore: idx,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	mustPut(t, c, "a.txt", randBytes(16))
	mustPut(t, c, "b.txt", randBytes(16))
	c.Flush()

	// The store is empty and ready; if the listing consulted it, the page would
	// come back empty instead of merged from the nodes.
	req := fs.ListObjectsRequest{Bucket: "b"}
	merged, walked := listBoth(t, c, &req)

	assert.Equal(t, keysOf(walked), keysOf(merged))
	assert.Equal(t, []string{"a.txt", "b.txt"}, keysOf(merged))
}
