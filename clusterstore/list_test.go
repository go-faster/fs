package clusterstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

func TestBucketRegistry(t *testing.T) {
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	// Absent bucket.
	ok, err := c.BucketExists(t.Context(), "b1")
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = c.Bucket(t.Context(), "b1")
	require.ErrorIs(t, err, fs.ErrBucketNotFound)
	require.ErrorIs(t, c.DeleteBucket(t.Context(), "b1"), fs.ErrBucketNotFound)
	require.ErrorIs(t, c.SetBucketACL(t.Context(), "b1", fs.ACLPublicRead), fs.ErrBucketNotFound)

	// Create + read back.
	require.NoError(t, c.CreateBucket(t.Context(), "b1", fs.ACLPrivate))
	require.ErrorIs(t, c.CreateBucket(t.Context(), "b1", fs.ACLPrivate), fs.ErrBucketAlreadyExists)

	ok, err = c.BucketExists(t.Context(), "b1")
	require.NoError(t, err)
	assert.True(t, ok)

	info, err := c.Bucket(t.Context(), "b1")
	require.NoError(t, err)
	assert.Equal(t, "b1", info.Name)
	assert.Equal(t, fs.ACLPrivate, info.ACL)
	assert.False(t, info.Created.IsZero())

	// ACL rewrite.
	require.NoError(t, c.SetBucketACL(t.Context(), "b1", fs.ACLPublicRead))

	info, err = c.Bucket(t.Context(), "b1")
	require.NoError(t, err)
	assert.Equal(t, fs.ACLPublicRead, info.ACL)

	// Listing is deduplicated (records replicate 3-way) and sorted.
	require.NoError(t, c.CreateBucket(t.Context(), "a-first", fs.ACLPrivate))

	buckets, err := c.ListBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 2)
	assert.Equal(t, "a-first", buckets[0].Name)
	assert.Equal(t, "b1", buckets[1].Name)

	// Delete removes every replica.
	require.NoError(t, c.DeleteBucket(t.Context(), "b1"))

	buckets, err = c.ListBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)

	require.NoError(t, c.DeleteBucket(t.Context(), "a-first"))
	assert.Empty(t, fc.allNames(), "no record replicas may remain")
}

func TestBucketSurvivesNodeLoss(t *testing.T) {
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	targets := placement.Place(fc.topo, bucketPlacementKey("b"), bucketCopies)
	require.Len(t, targets, bucketCopies)

	// Any single record holder down: existence, fetch and listing still work.
	fc.setDown(targets[0].Node, true)

	ok, err := c.BucketExists(t.Context(), "b")
	require.NoError(t, err)
	assert.True(t, ok)

	buckets, err := c.ListBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, "b", buckets[0].Name)
}

func TestListObjects(t *testing.T) {
	for _, s := range testSchemes() {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(6, 2)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

			keys := []string{"a/1", "a/2", "b/1", "solo", "префикс/ключ"}
			for _, key := range keys {
				mustPut(t, c, key, randBytes(1000))
			}

			c.Flush()

			// Full listing: sorted, one entry per key despite replicated
			// sidecars.
			all, err := c.ListObjects(t.Context(), "b", "")
			require.NoError(t, err)
			require.Len(t, all, len(keys))

			for i, sc := range all {
				assert.Equal(t, "b", sc.Bucket)
				assert.Equal(t, int64(1000), sc.Size)

				if i > 0 {
					assert.Less(t, all[i-1].Key, sc.Key, "sorted by key")
				}
			}

			// Prefix filter.
			as, err := c.ListObjects(t.Context(), "b", "a/")
			require.NoError(t, err)
			require.Len(t, as, 2)
			assert.Equal(t, "a/1", as[0].Key)
			assert.Equal(t, "a/2", as[1].Key)

			// Other buckets are invisible.
			other, err := c.ListObjects(t.Context(), "other", "")
			require.NoError(t, err)
			assert.Empty(t, other)

			// A deleted object leaves the listing.
			require.NoError(t, c.Delete(t.Context(), "b", "solo"))

			all, err = c.ListObjects(t.Context(), "b", "")
			require.NoError(t, err)
			assert.Len(t, all, len(keys)-1)
		})
	}
}

func TestListObjectsNewestWins(t *testing.T) {
	fc := newFakeCluster(6, 1)
	c := fc.coordinator(t, Config{})

	first := mustPut(t, c, "k", randBytes(100))
	// Ensure a strictly newer Modified stamp even on coarse clocks.
	time.Sleep(2 * time.Millisecond)

	second := mustPut(t, c, "k", randBytes(200))

	// Do NOT flush: the old generation may still linger on some targets; the
	// merge must still surface only the newest write.
	all, err := c.ListObjects(t.Context(), "b", "")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, second.Generation, all[0].Generation)
	assert.Equal(t, int64(200), all[0].Size)
	assert.NotEqual(t, first.Generation, all[0].Generation)

	c.Flush()
}

func TestListObjectsSurvivesNodeLoss(t *testing.T) {
	s := scheme.Scheme{Kind: scheme.RF25}
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

	data := randBytes(500)
	mustPut(t, c, "k", data)
	c.Flush()

	plan, err := fragment.Plan(fc.topo, s, placement.ObjectKey("b", "k"), int64(len(data)))
	require.NoError(t, err)

	// One sidecar holder down: the other replicas keep the listing complete.
	fc.setDown(plan[0].Target.Node, true)

	all, err := c.ListObjects(t.Context(), "b", "")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "k", all[0].Key)

	// Every scan failing surfaces an error instead of an empty listing.
	for _, n := range fc.topo.Nodes {
		fc.setDown(n.ID, true)
	}

	_, err = c.ListObjects(t.Context(), "b", "")
	require.Error(t, err)
}
