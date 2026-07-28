package etcd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/etcd"
)

// TestMetaRebuildElectionSingleRunner: a cluster-wide rebuild has exactly one
// runner, and a standby takes over from the cursor rather than starting again.
func TestMetaRebuildElectionSingleRunner(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	a, err := etcd.CampaignMetaRebuild(t.Context(), client, cfg, "runner-a")
	require.NoError(t, err)

	type won struct {
		lead *etcd.MetaRebuildLeadership
		err  error
	}

	bCh := make(chan won, 1)

	go func() {
		lead, err := etcd.CampaignMetaRebuild(t.Context(), client, cfg, "runner-b")
		bCh <- won{lead, err}
	}()

	select {
	case <-bCh:
		t.Fatal("second candidate must not win while the first holds leadership")
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, a.SaveCursor(t.Context(), `{"bucket":"photos","key":"a.jpg"}`))

	val, ok, err := etcd.LoadMetaRebuildCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok, "a rebuild in progress must be visible to the standby")
	assert.Equal(t, `{"bucket":"photos","key":"a.jpg"}`, val)

	// Resigning lets the standby in without waiting out the lease.
	require.NoError(t, a.Close())

	var b won

	select {
	case b = <-bCh:
	case <-time.After(5 * time.Second):
		t.Fatal("standby did not win after the holder resigned")
	}

	require.NoError(t, b.err)
	t.Cleanup(func() { _ = b.lead.Close() })

	// The cursor survives the handover — that is the whole point of persisting
	// it, and the standby resumes rather than emptying the store.
	val, ok, err = etcd.LoadMetaRebuildCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"bucket":"photos","key":"a.jpg"}`, val)
}

// TestMetaRebuildCursorIsFenced: a deposed runner's late write must not move
// the cursor. Here that is worse than a stale value — the old runner's cursor
// is *behind* the new one's, so accepting it would re-index everything between
// and, on a store that had already been emptied, could look like progress going
// backwards.
func TestMetaRebuildCursorIsFenced(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	a, err := etcd.CampaignMetaRebuild(t.Context(), client, cfg, "runner-a")
	require.NoError(t, err)

	require.NoError(t, a.SaveCursor(t.Context(), "from-a"))
	require.NoError(t, a.Close())

	b, err := etcd.CampaignMetaRebuild(t.Context(), client, cfg, "runner-b")
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	require.NoError(t, b.SaveCursor(t.Context(), "from-b"))

	// A no longer holds leadership; its write is rejected rather than
	// clobbering B's progress.
	require.Error(t, a.SaveCursor(t.Context(), "stale"),
		"a resigned runner must not write the cursor")

	val, ok, err := etcd.LoadMetaRebuildCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "from-b", val, "the current leader's cursor stands")
}

// TestMetaRebuildClearCursorEndsTheRebuild: an absent cursor is what tells the
// next runner to start fresh, so clearing is how a completed rebuild is
// recorded — and it must only happen once the walk is genuinely done.
func TestMetaRebuildClearCursorEndsTheRebuild(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	lead, err := etcd.CampaignMetaRebuild(t.Context(), client, cfg, "runner")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lead.Close() })

	require.NoError(t, lead.SaveCursor(t.Context(), "mid-walk"))

	_, ok, err := etcd.LoadMetaRebuildCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, lead.ClearCursor(t.Context()))

	_, ok, err = etcd.LoadMetaRebuildCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.False(t, ok, "no cursor means no rebuild in progress")
}

// TestMetaRebuildIsASeparateElection: a cluster rebuilding its metadata plane
// cannot serve listings from it, so the rebuild must not queue behind a
// fragment relocation that may take hours.
func TestMetaRebuildIsASeparateElection(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	rebalance, err := etcd.CampaignRebalance(t.Context(), client, cfg, "rebalancer")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rebalance.Close() })

	done := make(chan error, 1)

	go func() {
		lead, err := etcd.CampaignMetaRebuild(t.Context(), client, cfg, "rebuilder")
		if err == nil {
			t.Cleanup(func() { _ = lead.Close() })
		}

		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "the rebuild must not wait on the rebalance election")
	case <-time.After(5 * time.Second):
		t.Fatal("metadata rebuild queued behind the rebalance leadership")
	}
}
