package etcd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

func testMap() *rangemap.Map {
	return &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "om", Owner: "n0", Followers: []cluster.NodeID{"n1"}},
		{Start: "om", End: "ot", Owner: "n1", Followers: []cluster.NodeID{"n2"}},
		{Start: "ot", End: "", Owner: "n2"},
	}}
}

func TestRangeMapRoundTrip(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	_, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.False(t, ok, "an uninitialized plane is distinguishable from a broken one")

	want := testMap()
	require.NoError(t, etcd.SaveRangeMap(t.Context(), client, cfg, want))

	got, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)

	assert.Equal(t, want.Ranges, got.Ranges)
	assert.Positive(t, got.Revision, "the revision is what routing compares against")
}

// TestRangeMapBoundariesSurviveArbitraryBytes: a boundary is a position in the
// object key space, so it can hold anything a bucket name and object key can —
// including bytes that are not valid in an etcd key or in UTF-8. Hex encoding
// is what makes that safe, and this is the case that fails without it.
func TestRangeMapBoundariesSurviveArbitraryBytes(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	want := &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "o\x00\xff", Owner: "n0"},
		{Start: "o\x00\xff", End: "o\xff\xfe", Owner: "n1"},
		{Start: "o\xff\xfe", End: "", Owner: "n2"},
	}}
	require.NoError(t, etcd.SaveRangeMap(t.Context(), client, cfg, want))

	got, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, want.Ranges, got.Ranges)
}

// TestRangeMapIsReturnedSorted: the map has to come back as a partition, and a
// prefix read returns keys in etcd's byte order — which hex preserves. If it
// did not, Validate would reject a map that is actually fine, or worse accept
// one that is not.
func TestRangeMapIsReturnedSorted(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	m, err := rangemap.Initial(16, []cluster.NodeID{"n0", "n1", "n2"})
	require.NoError(t, err)
	require.NoError(t, etcd.SaveRangeMap(t.Context(), client, cfg, m))

	got, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, got.Validate(), "what comes back must still be a partition")
	assert.Equal(t, m.Ranges, got.Ranges)
}

// TestSaveRangeMapReplaces: a range that used to exist and no longer does must
// not survive as an orphan. It would overlap its successor, and an overlap is
// two nodes each believing they own the same keys.
func TestSaveRangeMapReplaces(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	require.NoError(t, etcd.SaveRangeMap(t.Context(), client, cfg, testMap()))

	fewer := &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "", Owner: "n0"},
	}}
	require.NoError(t, etcd.SaveRangeMap(t.Context(), client, cfg, fewer))

	got, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, fewer.Ranges, got.Ranges, "the old boundaries are gone, not merged")
}

// TestSaveRangeMapRefusesAGap: writing a non-partition would be a key nothing
// owns, which does not error at lookup time — it routes to the range before it,
// whose owner does not hold the key, so the object reads as absent.
func TestSaveRangeMapRefusesAGap(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	broken := &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "om", Owner: "n0"},
		{Start: "op", End: "", Owner: "n1"},
	}}

	require.ErrorContains(t, etcd.SaveRangeMap(t.Context(), client, cfg, broken), "gap or overlap")

	_, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.False(t, ok, "a refused save leaves nothing behind")
}

// TestSaveRangeIsFencedOnTheRevision is the ordinary change — a promotion, a
// move, a follower set update — and the reason it is fenced: two controllers
// acting on the same map must not silently overwrite each other, because the
// loser's view of who owns what would then be wrong and it would not know.
func TestSaveRangeIsFencedOnTheRevision(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	require.NoError(t, etcd.SaveRangeMap(t.Context(), client, cfg, testMap()))

	m, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)

	// Both controllers read the same map; the first promotion wins.
	promoted := m.Ranges[1]
	promoted.Owner = "n2"
	require.NoError(t, etcd.SaveRange(t.Context(), client, cfg, promoted, m.Revision))

	// The second acts on what it read, and is told the world moved.
	stale := m.Ranges[1]
	stale.Owner = "n0"
	require.ErrorContains(t, etcd.SaveRange(t.Context(), client, cfg, stale, m.Revision), "changed since")

	after, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.EqualValues(t, "n2", after.Ranges[1].Owner, "the first writer stands")
	assert.Greater(t, after.Revision, m.Revision, "the revision moved, so routers refetch")
}

// TestSaveRangeSucceedsOnACurrentRead: the fence must not reject a caller that
// is up to date, or ownership could never change at all.
func TestSaveRangeSucceedsOnACurrentRead(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	require.NoError(t, etcd.SaveRangeMap(t.Context(), client, cfg, testMap()))

	for range 3 {
		m, ok, err := etcd.LoadRangeMap(t.Context(), client, cfg)
		require.NoError(t, err)
		require.True(t, ok)

		r := m.Ranges[0]
		r.Followers = append(r.Followers, "n2")
		require.NoError(t, etcd.SaveRange(t.Context(), client, cfg, r, m.Revision))
	}
}
