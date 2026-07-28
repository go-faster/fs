package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// indexBucket is the bucket every case here writes to.
const indexBucket = "photos"

// indexNode boots one full cluster node against the given etcd endpoint,
// keeping its storage root so a restart can reuse the same index.
func indexNode(t *testing.T, endpoint, prefix, root string, index int) *clusterRuntime {
	t.Helper()

	addr := testFreeAddr(t)

	cfg := validClusterConfig()
	cfg.Cluster.NodeID = "n" + strconv.Itoa(index)
	cfg.Cluster.Rack = "r" + strconv.Itoa(index)
	cfg.Cluster.Addr = addr
	cfg.Cluster.AdvertiseAddr = addr
	cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: prefix, TTL: 2 * time.Second}
	cfg.Cluster.Disks = []ClusterDiskConfig{
		{ID: "d0", Path: filepath.Join(root, "d0")},
	}
	cfg.Storage.Fsync = "none"
	require.NoError(t, cfg.Validate())

	rt, err := buildCluster(t.Context(), zaptest.NewLogger(t), cfg, root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.close() })

	return rt
}

// indexedKeys reads the test bucket's keys out of a node's index.
func indexedKeys(t *testing.T, rt *clusterRuntime) []string {
	t.Helper()

	var keys []string

	require.NoError(t, rt.index.Scan(t.Context(), indexBucket, "", "", 0, func(e metastore.Entry) error {
		keys = append(keys, e.Key)

		return nil
	}))

	return keys
}

// indexCluster boots three nodes, which is the smallest cluster that can hold
// a bucket record: those need two distinct failure domains.
func indexCluster(t *testing.T, prefix string) []*clusterRuntime {
	t.Helper()

	endpoint := startTestEtcd(t)

	grp, grpCtx := errgroup.WithContext(t.Context())

	nodes := make([]*clusterRuntime, 3)
	for i := range nodes {
		nodes[i] = indexNode(t, endpoint, prefix, t.TempDir(), i)

		grp.Go(func() error { return nodes[i].Serve(grpCtx) })
	}

	require.Eventually(t, func() bool {
		return nodes[0].coord.Topology().DiskCount() == 3
	}, 15*time.Second, 20*time.Millisecond, "topology must converge")

	return nodes
}

// putObjects writes size-byte objects through the first node.
func putObjects(t *testing.T, rt *clusterRuntime, size int, keys ...string) {
	t.Helper()

	for _, key := range keys {
		_, err := rt.Storage.PutObject(t.Context(), &fs.PutObjectRequest{
			Bucket: indexBucket, Key: key, Reader: bytes.NewReader(make([]byte, size)), Size: int64(size),
		})
		require.NoError(t, err)
	}

	rt.coord.Flush()
}

// TestObjectIndexFollowsTheWritePath drives a real three-node cluster and
// checks each node indexes what its own disks took — which is the whole point
// of hanging the index off the store rather than off the coordinator: a node
// holds replicas placed by other nodes, and those must be indexed too.
func TestObjectIndexFollowsTheWritePath(t *testing.T) {
	endpoint := startTestEtcd(t)
	prefix := "/fs-objindex"

	grp, grpCtx := errgroup.WithContext(t.Context())

	nodes := make([]*clusterRuntime, 3)
	for i := range nodes {
		nodes[i] = indexNode(t, endpoint, prefix, t.TempDir(), i)

		grp.Go(func() error { return nodes[i].Serve(grpCtx) })
	}

	require.Eventually(t, func() bool {
		return nodes[0].coord.Topology().DiskCount() == 3
	}, 15*time.Second, 20*time.Millisecond, "topology must converge")

	storage := nodes[0].Storage
	require.NoError(t, storage.CreateBucket(t.Context(), "photos"))

	put := func(key string, size int) {
		t.Helper()

		_, err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
			Bucket: "photos", Key: key, Reader: bytes.NewReader(make([]byte, size)), Size: int64(size),
		})
		require.NoError(t, err)
	}

	put("a.jpg", 1000)
	put("b.jpg", 2000)

	nodes[0].coord.Flush()

	// Every object is replicated, so each node indexes the ones it holds and
	// the union covers the bucket. Summing objects across nodes would count an
	// object once per replica — attribution is the cluster-usage problem, not
	// this one.
	total := map[string]bool{}

	for _, rt := range nodes {
		for _, key := range indexedKeys(t, rt) {
			total[key] = true
		}
	}

	assert.Equal(t, map[string]bool{"a.jpg": true, "b.jpg": true}, total,
		"between them the nodes index every object")

	// An overwrite replaces rather than adds, on whichever nodes hold it.
	put("a.jpg", 50)
	nodes[0].coord.Flush()

	for _, rt := range nodes {
		entry, found, err := rt.index.Get(t.Context(), "photos", "a.jpg")
		require.NoError(t, err)

		if !found {
			continue
		}

		assert.Equal(t, int64(50), entry.Size, "node %s has the new size", rt.nodeID)
	}

	// A delete removes it everywhere it was held.
	require.NoError(t, storage.DeleteObject(t.Context(), "photos", "b.jpg"))

	for _, rt := range nodes {
		_, found, err := rt.index.Get(t.Context(), "photos", "b.jpg")
		require.NoError(t, err)
		assert.False(t, found, "node %s still indexes a deleted object", rt.nodeID)
	}
}

// TestObjectIndexRebuildsFromDisks covers the recovery path that makes
// unsynced index writes safe: a node whose index was not handed over cleanly
// rebuilds it from the records its disks still hold.
//
// The invariant is per node — a rebuild reproduces what that node's disks hold,
// which under replication is a share of the bucket, not all of it.
func TestObjectIndexRebuildsFromDisks(t *testing.T) {
	nodes := indexCluster(t, "/fs-objindex-rebuild")
	rt := nodes[0]

	require.NoError(t, rt.Storage.CreateBucket(t.Context(), "photos"))
	putObjects(t, rt, 100, "a.jpg", "b.jpg", "c.jpg")

	held := indexedKeys(t, rt)
	require.NotEmpty(t, held, "this node must hold something to rebuild")

	before, err := rt.index.Usage(t.Context(), "photos")
	require.NoError(t, err)

	// Wipe the index behind the node's back — a corrupt or discarded index,
	// which is exactly what a rebuild exists for.
	require.NoError(t, rt.index.Reset(t.Context()))
	require.Empty(t, indexedKeys(t, rt))

	state, err := rt.index.State(t.Context())
	require.NoError(t, err)
	require.Equal(t, metastore.StateBuilding, state)

	// The startup path finds it unusable and rebuilds from the disks.
	rt.RunObjectIndex(t.Context())

	assert.Equal(t, held, indexedKeys(t, rt), "the rebuild reproduces what the disks hold")

	after, err := rt.index.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, before, after, "and the counters it carries")

	state, err = rt.index.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateReady, state, "a completed build makes it usable")
}

// TestObjectIndexSkipsUnreadableRecords: one bad record must not abandon a
// build over millions of good ones.
func TestObjectIndexSkipsUnreadableRecords(t *testing.T) {
	nodes := indexCluster(t, "/fs-objindex-bad")
	rt := nodes[0]

	require.NoError(t, rt.Storage.CreateBucket(t.Context(), "photos"))
	putObjects(t, rt, 10, "a.jpg", "b.jpg", "c.jpg", "d.jpg")

	held := indexedKeys(t, rt)
	require.NotEmpty(t, held)

	// A commit record that decodes to nothing usable, written straight to the
	// disk the way a corruption would leave it.
	w, err := rt.store.Create(t.Context(), "d0", "obj/deadbeef/deadbeef/meta")
	require.NoError(t, err)

	_, err = w.Write([]byte("not json"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	require.NoError(t, rt.buildObjectIndex(t.Context()))

	assert.Equal(t, held, indexedKeys(t, rt),
		"the readable records are indexed and the broken one is skipped")
}

// TestObjectIndexIgnoresBucketRecords pins the distinction that a suffix match
// alone gets wrong: bucket records live under "bkt/" and end in "/meta" too.
// Feeding them to an object index fails to decode them and — worse — counts
// them as records the index missed, which is the signal that it has fallen
// behind and needs a rebuild.
func TestObjectIndexIgnoresBucketRecords(t *testing.T) {
	assert.True(t, isObjectRecord("obj/aa/bb/meta"))
	assert.False(t, isObjectRecord("bkt/aa/meta"), "a bucket record is not an object")
	assert.False(t, isObjectRecord("obj/aa/bb/gen1.f0"), "a payload fragment carries no identity")

	nodes := indexCluster(t, "/fs-objindex-buckets")
	rt := nodes[0]

	indexer := newObjectIndexer(rt.index, zaptest.NewLogger(t))
	assert.False(t, indexer.Wants("bkt/aa/meta"))

	// Creating buckets writes bucket records to every node; none of them may
	// register as a miss.
	for _, bucket := range []string{"photos", "logs", "backups"} {
		require.NoError(t, rt.Storage.CreateBucket(t.Context(), bucket))
	}

	putObjects(t, rt, 10, "a.jpg")

	for _, node := range nodes {
		assert.Zero(t, node.indexer.Dropped(), "node %s counted a record as missed", node.nodeID)
	}
}

// TestListingServedFromTheIndex drives a real cluster — real pebble indexes,
// real peer transport — and checks a listing comes back from them rather than
// from the sidecar walk, with the same answer the walk gives.
func TestListingServedFromTheIndex(t *testing.T) {
	nodes := indexCluster(t, "/fs-index-listing")
	rt := nodes[0]

	require.NoError(t, rt.Storage.CreateBucket(t.Context(), indexBucket))
	putObjects(t, rt, 32,
		"a.txt", "docs/one.txt", "docs/two.txt", "docs/deep/three.txt", "images/x.png", "z.txt")

	// Every node's index must be usable before a listing can be served from
	// them; a node still building one makes the listing fall back.
	for _, node := range nodes {
		node.RunObjectIndex(t.Context())
	}

	require.Eventually(t, func() bool {
		for _, node := range nodes {
			state, err := node.index.State(t.Context())
			if err != nil || state != metastore.StateReady {
				return false
			}
		}

		return true
	}, 10*time.Second, 50*time.Millisecond, "indexes must become ready")

	// Flat listing.
	page, err := rt.Storage.ListObjects(t.Context(), &fs.ListObjectsRequest{Bucket: indexBucket})
	require.NoError(t, err)

	var keys []string
	for _, o := range page.Objects {
		keys = append(keys, o.Key)
	}

	assert.Equal(t, []string{
		"a.txt", "docs/deep/three.txt", "docs/one.txt", "docs/two.txt", "images/x.png", "z.txt",
	}, keys)

	// Folded listing: the deep prefix collapses, and sizes survive the merge.
	folded, err := rt.Storage.ListObjects(t.Context(), &fs.ListObjectsRequest{
		Bucket: indexBucket, Delimiter: "/",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"docs/", "images/"}, folded.CommonPrefixes)
	require.Len(t, folded.Objects, 2)
	assert.Equal(t, int64(32), folded.Objects[0].Size)

	// Paging through the bucket sees each key once, in order.
	var (
		paged []string
		after string
	)

	for {
		p, err := rt.Storage.ListObjects(t.Context(), &fs.ListObjectsRequest{
			Bucket: indexBucket, StartAfter: after, Limit: 2,
		})
		require.NoError(t, err)

		for _, o := range p.Objects {
			paged = append(paged, o.Key)
		}

		if !p.IsTruncated {
			break
		}

		after = p.NextStartAfter
	}

	assert.Equal(t, keys, paged)
}

// TestRecountUsesTheIndex drives the usage correction on a real cluster and
// checks it derives the totals from the indexes rather than from a walk of every
// sidecar — and that the answer is the same either way.
func TestRecountUsesTheIndex(t *testing.T) {
	nodes := indexCluster(t, "/fs-index-usage")
	rt := nodes[0]

	require.NoError(t, rt.Storage.CreateBucket(t.Context(), indexBucket))
	putObjects(t, rt, 64, "a.jpg", "b.jpg", "c.jpg")

	for _, node := range nodes {
		node.RunObjectIndex(t.Context())
	}

	// Before the indexes are usable the walk still answers, which is what keeps
	// a rolling upgrade working.
	walked, err := rt.coord.CountObjects(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(3), walked[indexBucket].Objects)

	require.Eventually(t, func() bool {
		_, err := rt.coord.CountObjectsIndexed(t.Context())

		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the indexes must become usable")

	indexed, err := rt.coord.CountObjectsIndexed(t.Context())
	require.NoError(t, err)
	assert.Equal(t, walked, indexed, "both paths count the same objects")

	// The recount now prefers the indexes, and writes what it found.
	fromIndex, err := rt.recountUsage(t.Context())
	require.NoError(t, err)
	assert.True(t, fromIndex, "the recount read the indexes")

	rec, present, err := etcd.LoadBucketUsage(t.Context(), rt.client,
		etcd.Config{Prefix: "/fs-index-usage", TTL: 2}, indexBucket)
	require.NoError(t, err)
	require.True(t, present)
	assert.Equal(t, int64(3), rec.Objects)
	assert.Equal(t, int64(192), rec.Bytes)
	assert.False(t, rec.Counted.IsZero(), "and stamped when it anchored them")
}

// TestScrubCoverageEndToEnd drives a real node's scrub and checks the coverage
// it reports: nothing verified before a pass, everything the node holds
// verified after one, and the numbers reaching the live status a peer reads.
func TestScrubCoverageEndToEnd(t *testing.T) {
	nodes := indexCluster(t, "/fs-index-scrub")
	rt := nodes[0]

	require.NoError(t, rt.Storage.CreateBucket(t.Context(), indexBucket))
	putObjects(t, rt, 48, "a.jpg", "b.jpg", "c.jpg", "d.jpg")

	for _, node := range nodes {
		node.RunObjectIndex(t.Context())
	}

	cov, err := rt.index.Coverage(t.Context())
	require.NoError(t, err)
	require.Positive(t, cov.Objects, "the node must hold something")
	assert.Equal(t, cov.Objects, cov.Never, "nothing has been verified yet")
	assert.True(t, cov.Oldest.IsZero())

	// A scrub pass verifies what this node holds and stamps it.
	report, err := rt.repairer.Scrub(t.Context())
	require.NoError(t, err)
	require.Positive(t, report.Objects)

	cov, err = rt.index.Coverage(t.Context())
	require.NoError(t, err)
	assert.Zero(t, cov.Never, "a completed pass leaves nothing unverified")
	assert.False(t, cov.Oldest.IsZero(), "and the coverage has an age")

	// The status a peer reads carries it, but only after the background
	// refresh — the request path must not scan the index itself.
	before := rt.nodeStatusNow(t)
	assert.Zero(t, before.Scrub.Held, "coverage is not derived on the status path")

	ctx, cancel := context.WithCancel(t.Context())
	go rt.RunCoverage(ctx)

	require.Eventually(t, func() bool {
		return rt.nodeStatusNow(t).Scrub.Held > 0
	}, 5*time.Second, 20*time.Millisecond, "the refresh must publish coverage")

	cancel()

	status := rt.nodeStatusNow(t)
	assert.Equal(t, cov.Objects, status.Scrub.Held)
	assert.Zero(t, status.Scrub.NeverVerified)
	assert.False(t, status.Scrub.OldestVerified.IsZero())
}

// nodeStatusNow reads the node's live state the way a peer would.
func (rt *clusterRuntime) nodeStatusNow(t *testing.T) transport.NodeStatus {
	t.Helper()

	st, err := rt.nodeStatus(t.Context())
	require.NoError(t, err)

	return st
}
