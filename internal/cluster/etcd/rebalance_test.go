package etcd_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/etcd"
)

func TestRebalanceElectionSingleRunner(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	// First candidate wins immediately.
	a, err := etcd.CampaignRebalance(t.Context(), client, cfg, "runner-a")
	require.NoError(t, err)

	// Second candidate blocks while A holds the slot.
	type won struct {
		lead *etcd.RebalanceLeadership
		err  error
	}

	bCh := make(chan won, 1)

	go func() {
		lead, err := etcd.CampaignRebalance(t.Context(), client, cfg, "runner-b")
		bCh <- won{lead, err}
	}()

	select {
	case <-bCh:
		t.Fatal("second candidate must not win while the first holds leadership")
	case <-time.After(300 * time.Millisecond):
	}

	// The holder checkpoints its cursor.
	require.NoError(t, a.SaveCursor(t.Context(), "cursor-1"))

	val, ok, err := etcd.LoadRebalanceCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "cursor-1", val)

	// A releases: B takes over and reads A's cursor.
	require.NoError(t, a.Close())

	var b won
	select {
	case b = <-bCh:
	case <-time.After(10 * time.Second):
		t.Fatal("second candidate did not take over after the first resigned")
	}

	require.NoError(t, b.err)

	val, ok, err = etcd.LoadRebalanceCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "cursor-1", val)

	// A deposed runner's late cursor writes are fenced off.
	require.Error(t, a.SaveCursor(t.Context(), "stale"), "closed leadership must not write the cursor")

	val, ok, err = etcd.LoadRebalanceCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "cursor-1", val, "fenced write must not clobber the cursor")

	// B finishes the walk and clears the cursor.
	require.NoError(t, b.lead.ClearCursor(t.Context()))

	_, ok, err = etcd.LoadRebalanceCursor(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, b.lead.Close())
}

func TestCampaignRebalanceContextCanceled(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/test", TTL: 2}

	hold, err := etcd.CampaignRebalance(t.Context(), client, cfg, "holder")
	require.NoError(t, err)

	defer func() { require.NoError(t, hold.Close()) }()

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	_, err = etcd.CampaignRebalance(ctx, client, cfg, "late")
	require.Error(t, err, "campaign must give up when its context ends")
}

func TestLoadRebalanceCursorEmpty(t *testing.T) {
	client := startEtcd(t)

	_, ok, err := etcd.LoadRebalanceCursor(t.Context(), client, etcd.Config{Prefix: "/test"})
	require.NoError(t, err)
	assert.False(t, ok)
}
