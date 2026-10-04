package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
)

// TestSweepsBucket: in a cluster exactly one node sweeps each bucket, buckets
// spread over the nodes, and a node that is down hands its buckets on.
func TestSweepsBucket(t *testing.T) {
	ids := []layout.NodeID{"a", "b", "c"}

	var roles []layout.Node
	for _, id := range ids {
		roles = append(roles, layout.Node{ID: id, Zone: "z-" + string(id), Capacity: 1})
	}

	l, err := layout.Compute(nil, roles, layout.Options{Partitions: 16})
	require.NoError(t, err)

	members := make([]*peer.Member, len(ids))
	servers := make([]*httptest.Server, len(ids))

	for i, id := range ids {
		srv := httptest.NewUnstartedServer(nil)
		cfg := peer.Config{ID: id, Addr: srv.Listener.Addr().String(), Secret: peer.Secret("cluster-secret-0123456789"), Dir: t.TempDir()}

		if i > 0 {
			cfg.Peers = []string{servers[0].Listener.Addr().String()}
		}

		m, err := peer.New(cfg)
		require.NoError(t, err)

		_, err = m.Adopt(l)
		require.NoError(t, err)

		srv.Config.Handler = m.Handler()
		srv.Start()
		t.Cleanup(srv.Close)

		members[i], servers[i] = m, srv
	}

	gossip := func(live []*peer.Member) {
		for range 3 {
			for _, m := range live {
				m.Round(context.Background())
			}
		}
	}

	owners := func(live []*peer.Member) map[layout.NodeID]int {
		per := map[layout.NodeID]int{}

		for i := range 64 {
			bucket := fmt.Sprint("bucket-", i)
			claimed := 0

			for _, m := range live {
				if sweepsBucket(m, bucket) {
					claimed++
					per[m.ID()]++
				}
			}

			require.Equal(t, 1, claimed, "bucket %s has exactly one sweeper", bucket)
		}

		return per
	}

	require.Eventually(t, func() bool {
		gossip(members)

		for _, m := range members {
			for _, p := range m.Peers() {
				if p.Err != nil || p.Seen.IsZero() {
					return false
				}
			}

			if len(m.Peers()) < 2 {
				return false
			}
		}

		return true
	}, 5*time.Second, 10*time.Millisecond)

	per := owners(members)
	assert.Len(t, per, 3, "buckets spread over every node")

	// c goes down: the others notice and take its buckets.
	servers[2].Close()
	gossip(members[:2])

	per = owners(members[:2])
	assert.Equal(t, 64, per["a"]+per["b"])
}
