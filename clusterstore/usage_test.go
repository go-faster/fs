package clusterstore

import (
	"bytes"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// recordingUsage collects the deltas a coordinator reports, the way the etcd
// reporter folds them before flushing.
type recordingUsage struct {
	mu      sync.Mutex
	totals  map[string]BucketTotals
	buckets []string
}

func newRecordingUsage() *recordingUsage {
	return &recordingUsage{totals: make(map[string]BucketTotals)}
}

func (r *recordingUsage) Observe(bucket string, objects, size int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, seen := r.totals[bucket]; !seen {
		r.buckets = append(r.buckets, bucket)
	}

	t := r.totals[bucket]
	t.Objects += objects
	t.Bytes += size
	r.totals[bucket] = t
}

func (r *recordingUsage) get(bucket string) BucketTotals {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.totals[bucket]
}

func (r *recordingUsage) observed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.buckets...)
}

// usageCluster builds a three-node cluster whose coordinator reports usage to
// the returned recorder.
func usageCluster(t *testing.T) (*Coordinator, *recordingUsage) {
	t.Helper()

	secret := transport.Secret(randBytes(32))
	topo := &cluster.Topology{Epoch: 1}
	stores := make(map[cluster.NodeID]*trackingStore)

	for i := range 3 {
		id := cluster.NodeID("n" + strconv.Itoa(i))
		store := newTrackingStore()
		srv := httptest.NewServer(transport.NewServer(store, secret))
		t.Cleanup(srv.Close)

		stores[id] = store
		topo.Nodes = append(topo.Nodes, cluster.Node{
			ID:    id,
			Addr:  srv.Listener.Addr().String(),
			Rack:  "r" + strconv.Itoa(i),
			Disks: []cluster.Disk{{ID: "d0", Weight: 1}},
		})
	}

	usage := newRecordingUsage()

	c, err := New(Config{
		Topology: StaticTopology{T: topo},
		Peers:    NewHTTPPeers("n0", stores["n0"], secret, nil),
		Usage:    usage,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c, usage
}

// TestUsageObservesWrites covers the accounting a bucket's totals are built
// from: a create adds an object, an overwrite moves only bytes, and a delete
// takes both back.
func TestUsageObservesWrites(t *testing.T) {
	c, usage := usageCluster(t)

	mustPut(t, c, "a.txt", randBytes(1000))
	assert.Equal(t, BucketTotals{Objects: 1, Bytes: 1000}, usage.get("b"))

	mustPut(t, c, "z.txt", randBytes(500))
	assert.Equal(t, BucketTotals{Objects: 2, Bytes: 1500}, usage.get("b"))

	// An overwrite replaces an object rather than adding one: the count holds
	// and only the size difference moves. Without this the totals would climb
	// forever on a bucket whose object set never changes.
	mustPut(t, c, "a.txt", randBytes(300))
	assert.Equal(t, BucketTotals{Objects: 2, Bytes: 800}, usage.get("b"))

	require.NoError(t, c.Delete(t.Context(), "b", "a.txt"))
	assert.Equal(t, BucketTotals{Objects: 1, Bytes: 500}, usage.get("b"))

	require.NoError(t, c.Delete(t.Context(), "b", "z.txt"))
	assert.Equal(t, BucketTotals{}, usage.get("b"), "an emptied bucket nets to zero")
}

// TestUsageSkipsInternalBuckets keeps multipart bookkeeping out of a bucket's
// accounting. Parts in flight are not objects — S3 does not list them or count
// them — and completion writes the assembled object through the normal path,
// so counting parts too would double every multipart upload and leave an
// abandoned one inflating the bucket forever.
func TestUsageSkipsInternalBuckets(t *testing.T) {
	c, usage := usageCluster(t)

	_, err := c.Put(t.Context(), &PutRequest{
		Bucket: partsBucket("b"), Key: "upload-1/00001", Size: 4, Body: bytes.NewReader([]byte("part")),
	})
	require.NoError(t, err)

	_, err = c.Put(t.Context(), &PutRequest{
		Bucket: uploadsBucket("b"), Key: "upload-1\x00a.txt", Size: 0, Body: bytes.NewReader(nil),
	})
	require.NoError(t, err)

	assert.Empty(t, usage.observed(), "no internal namespace is accounted for")
	assert.Equal(t, BucketTotals{}, usage.get(partsBucket("b")))

	// The completed object is.
	mustPut(t, c, "a.txt", randBytes(10))
	assert.Equal(t, []string{"b"}, usage.observed())
}

// TestCountObjects covers the authoritative recount: one scatter-gather over
// every disk, totalled per bucket, with replicas of the same object counted
// once.
func TestCountObjects(t *testing.T) {
	c, _ := usageCluster(t)

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	mustPut(t, c, "a.txt", randBytes(1000))
	mustPut(t, c, "b.txt", randBytes(2000))
	c.Flush()

	totals, err := c.CountObjects(t.Context())
	require.NoError(t, err)

	// Every object is replicated across targets, so a count that did not dedup
	// by key would report the replication factor instead of the object count.
	assert.Equal(t, BucketTotals{Objects: 2, Bytes: 3000}, totals["b"])

	// An overwrite leaves one object at the new size, not two.
	mustPut(t, c, "a.txt", randBytes(50))
	c.Flush()

	totals, err = c.CountObjects(t.Context())
	require.NoError(t, err)
	assert.Equal(t, BucketTotals{Objects: 2, Bytes: 2050}, totals["b"])

	require.NoError(t, c.Delete(t.Context(), "b", "b.txt"))

	totals, err = c.CountObjects(t.Context())
	require.NoError(t, err)
	assert.Equal(t, BucketTotals{Objects: 1, Bytes: 50}, totals["b"])
}

// TestCountObjectsSkipsInternalBuckets checks the recount applies the same rule
// the deltas do — otherwise every recount would contradict the accounting it is
// supposed to correct.
func TestCountObjectsSkipsInternalBuckets(t *testing.T) {
	c, _ := usageCluster(t)

	_, err := c.Put(t.Context(), &PutRequest{
		Bucket: partsBucket("b"), Key: "upload-1/00001", Size: 4, Body: bytes.NewReader([]byte("part")),
	})
	require.NoError(t, err)

	mustPut(t, c, "a.txt", randBytes(10))
	c.Flush()

	totals, err := c.CountObjects(t.Context())
	require.NoError(t, err)

	assert.Len(t, totals, 1, "only the user bucket is counted")
	assert.Equal(t, BucketTotals{Objects: 1, Bytes: 10}, totals["b"])
}

// TestUsageNilObserver checks a coordinator without accounting still works —
// the configuration every test cluster and every non-etcd deployment uses, so
// a nil observer must be a no-op rather than a panic on the write path.
func TestUsageNilObserver(t *testing.T) {
	c, _ := usageCluster(t)
	c.usage = nil

	mustPut(t, c, "a.txt", randBytes(10))
	require.NoError(t, c.Delete(t.Context(), "b", "a.txt"))
}
