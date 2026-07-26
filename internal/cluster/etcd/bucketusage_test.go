package etcd_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/etcd"
)

// TestBucketUsageDeltas covers the incremental path: the first delta creates
// the record, later ones accumulate, and a delete takes it back down.
func TestBucketUsageDeltas(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-bucket-usage", TTL: 2}

	_, present, err := etcd.LoadBucketUsage(t.Context(), client, cfg, "photos")
	require.NoError(t, err)
	assert.False(t, present, "a bucket nobody has written to has no record")

	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "photos", 1, 1024))

	rec, present, err := etcd.LoadBucketUsage(t.Context(), client, cfg, "photos")
	require.NoError(t, err)
	require.True(t, present)
	assert.Equal(t, "photos", rec.Bucket)
	assert.EqualValues(t, 1, rec.Objects)
	assert.EqualValues(t, 1024, rec.Bytes)
	assert.False(t, rec.Updated.IsZero())
	assert.True(t, rec.Counted.IsZero(), "no recount has anchored it yet")

	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "photos", 2, 2048))
	// An overwrite: no new object, only the size difference.
	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "photos", 0, -512))
	// A delete.
	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "photos", -1, -1024))

	rec, _, err = etcd.LoadBucketUsage(t.Context(), client, cfg, "photos")
	require.NoError(t, err)
	assert.EqualValues(t, 2, rec.Objects)
	assert.EqualValues(t, 1536, rec.Bytes)
}

// TestBucketUsageClampsNegative checks that accounting which has gone negative
// — a delete whose create was lost to a crash — reads as zero. A negative
// object count is never a truthful answer, and it would propagate into the
// cluster-wide total.
func TestBucketUsageClampsNegative(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-usage-clamp", TTL: 2}

	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "photos", -3, -900))

	rec, present, err := etcd.LoadBucketUsage(t.Context(), client, cfg, "photos")
	require.NoError(t, err)
	require.True(t, present)
	assert.Zero(t, rec.Objects)
	assert.Zero(t, rec.Bytes)
}

// TestBucketUsageConcurrentDeltas is why the update is a compare-and-set: two
// nodes accounting for their own writes must not lose each other's. A blind
// read-modify-write drops deltas here, silently and permanently.
func TestBucketUsageConcurrentDeltas(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-usage-cas", TTL: 2}

	const writers = 6

	var wg sync.WaitGroup

	errs := make([]error, writers)

	for i := range writers {
		wg.Go(func() {
			errs[i] = etcd.AddBucketUsage(t.Context(), client, cfg, "photos", 1, 100)
		})
	}

	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}

	rec, _, err := etcd.LoadBucketUsage(t.Context(), client, cfg, "photos")
	require.NoError(t, err)
	assert.EqualValues(t, writers, rec.Objects, "every writer's delta survived")
	assert.EqualValues(t, writers*100, rec.Bytes)
}

// TestBucketUsageRecountCarriesConcurrentDeltas covers the recount's central
// rule: a total derived from a walk must not discard what was accounted while
// the walk ran. Dropping those would bias every recount downward — the
// direction that hides data.
func TestBucketUsageRecountCarriesConcurrentDeltas(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-usage-recount", TTL: 2}

	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "photos", 10, 10_000))

	// What the recount read when it started.
	base, _, err := etcd.LoadBucketUsage(t.Context(), client, cfg, "photos")
	require.NoError(t, err)

	// Writes that land while the walk is in flight.
	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "photos", 2, 2_000))

	// The walk saw 9 objects — it started before those two, and one object had
	// been deleted without its delta ever being flushed.
	require.NoError(t, etcd.SetBucketUsage(t.Context(), client, cfg, "photos", 9, 9_000, base))

	rec, _, err := etcd.LoadBucketUsage(t.Context(), client, cfg, "photos")
	require.NoError(t, err)
	assert.EqualValues(t, 11, rec.Objects, "the walk's total plus what landed during it")
	assert.EqualValues(t, 11_000, rec.Bytes)
	assert.False(t, rec.Counted.IsZero(), "a recount stamps when it anchored the total")
}

// TestListBucketUsage covers the admin API's read path, including that a
// bucket's name survives the round trip through the key.
func TestListBucketUsage(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-usage-list", TTL: 2}

	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "zebra", 1, 10))
	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "alpha", 2, 20))
	require.NoError(t, etcd.AddBucketUsage(t.Context(), client, cfg, "my.dotted-bucket", 3, 30))

	list, err := etcd.ListBucketUsage(t.Context(), client, cfg)
	require.NoError(t, err)
	require.Len(t, list, 3)

	assert.Equal(t, "alpha", list[0].Bucket, "sorted by name")
	assert.Equal(t, "my.dotted-bucket", list[1].Bucket)
	assert.Equal(t, "zebra", list[2].Bucket)
	assert.EqualValues(t, 3, list[1].Objects)

	require.NoError(t, etcd.DeleteBucketUsage(t.Context(), client, cfg, "zebra"))

	list, err = etcd.ListBucketUsage(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

// TestCampaignUsageIsExclusive checks that only one node recounts at a time —
// the point of the election, since the recount reads every sidecar in the
// cluster and N nodes would do N times the work for one answer.
func TestCampaignUsageIsExclusive(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-usage-election", TTL: 2}

	first, err := etcd.CampaignUsage(t.Context(), client, cfg, "node-a")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	_, err = etcd.CampaignUsage(ctx, client, cfg, "node-b")
	require.Error(t, err, "a second candidate waits rather than running a parallel recount")

	require.NoError(t, first.Close())

	// With the slot released, the next candidate takes it without waiting out
	// the lease.
	second, err := etcd.CampaignUsage(t.Context(), client, cfg, "node-b")
	require.NoError(t, err)
	require.NoError(t, second.Close())
}
