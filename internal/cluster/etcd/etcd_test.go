package etcd_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// startEtcd runs an in-process single-node etcd and returns a client for it.
func startEtcd(t *testing.T) *clientv3.Client {
	t.Helper()

	cfg := embed.NewConfig()
	cfg.Dir = t.TempDir()
	cfg.LogLevel = "error"

	clientURL := url.URL{Scheme: "http", Host: freeAddr(t)}
	peerURL := url.URL{Scheme: "http", Host: freeAddr(t)}
	cfg.ListenClientUrls = []url.URL{clientURL}
	cfg.AdvertiseClientUrls = []url.URL{clientURL}
	cfg.ListenPeerUrls = []url.URL{peerURL}
	cfg.AdvertisePeerUrls = []url.URL{peerURL}
	cfg.InitialCluster = cfg.InitialClusterFromName(cfg.Name)

	srv, err := embed.StartEtcd(cfg)
	require.NoError(t, err)
	t.Cleanup(srv.Close)

	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		t.Fatal("etcd did not become ready")
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{clientURL.String()},
		DialTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// freeAddr reserves a localhost port.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

func testNode(i int) cluster.Node {
	return cluster.Node{
		ID:   cluster.NodeID("n" + strconv.Itoa(i)),
		Addr: "10.0.0." + strconv.Itoa(i) + ":7000",
		Rack: "r" + strconv.Itoa(i),
		Disks: []cluster.Disk{
			{ID: "d0", Weight: 1},
			{ID: "d1", Weight: 2},
		},
	}
}

// waitTopology polls the source until cond holds (watches are asynchronous).
func waitTopology(t *testing.T, s *etcd.Source, cond func(*cluster.Topology) bool) *cluster.Topology {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)

	for {
		topo := s.Topology()
		if topo != nil && cond(topo) {
			return topo
		}

		if time.Now().After(deadline) {
			t.Fatalf("topology never converged; last: %+v", topo)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestControlPlane(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-test", TTL: 2}

	// A source over an empty registry serves an empty topology.
	source, err := etcd.NewSource(t.Context(), client, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	topo := source.Topology()
	require.NotNil(t, topo)
	assert.Empty(t, topo.Nodes)

	// Three nodes register; the watch folds them in.
	regs := make([]*etcd.Registration, 3)
	for i := range regs {
		reg, err := etcd.Register(t.Context(), client, cfg, testNode(i))
		require.NoError(t, err)
		t.Cleanup(func() { _ = reg.Close() })

		regs[i] = reg
	}

	topo = waitTopology(t, source, func(tp *cluster.Topology) bool { return len(tp.Nodes) == 3 })
	assert.Equal(t, "n0", string(topo.Nodes[0].ID), "sorted by ID")
	assert.Equal(t, "r1", topo.Nodes[1].Rack)
	assert.Equal(t, "10.0.0.2:7000", topo.Nodes[2].Addr)
	assert.Equal(t, 2.0, topo.Nodes[0].Disks[1].Weight)
	assert.Positive(t, topo.Epoch)

	firstEpoch := topo.Epoch

	// A source started late (fresh Get, no events) sees the same members.
	late, err := etcd.NewSource(t.Context(), client, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = late.Close() })

	lateTopo := late.Topology()
	require.NotNil(t, lateTopo)
	assert.Len(t, lateTopo.Nodes, 3)

	// Registration outlives the lease TTL (keepalives refresh it).
	time.Sleep(3 * time.Second)
	assert.Len(t, source.Topology().Nodes, 3, "keepalive must outlive the TTL")

	// Re-registering updates the node in place (drained disk).
	drained := testNode(1)
	drained.Disks[0].Weight = 0

	reg, err := etcd.Register(t.Context(), client, cfg, drained)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })

	topo = waitTopology(t, source, func(tp *cluster.Topology) bool {
		return len(tp.Nodes) == 3 && tp.Nodes[1].Disks[0].Weight == 0
	})
	assert.Greater(t, topo.Epoch, firstEpoch, "epoch advances with the registry")

	// Deregistering (lease revoke) drops the node promptly.
	require.NoError(t, regs[0].Close())
	waitTopology(t, source, func(tp *cluster.Topology) bool {
		return len(tp.Nodes) == 2 && tp.Nodes[0].ID == "n1"
	})
}

// TestCoordinatorOverEtcd closes the loop: a clusterstore coordinator whose
// topology comes from the live etcd control plane.
func TestCoordinatorOverEtcd(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/fs-coord", TTL: 5}

	stores := make(map[cluster.NodeID]*transport.MemStore)

	for i := range 3 {
		node := testNode(i)
		stores[node.ID] = transport.NewMemStore()

		reg, err := etcd.Register(t.Context(), client, cfg, node)
		require.NoError(t, err)
		t.Cleanup(func() { _ = reg.Close() })
	}

	source, err := etcd.NewSource(t.Context(), client, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	waitTopology(t, source, func(tp *cluster.Topology) bool { return len(tp.Nodes) == 3 })

	coord, err := clusterstore.New(clusterstore.Config{
		Topology: source,
		Peers:    memPeers{stores: stores},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = coord.Close() })

	data := make([]byte, 50_000)
	_, _ = rand.Read(data)

	_, err = coord.Put(context.Background(), &clusterstore.PutRequest{
		Bucket: "b", Key: "k", Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	require.NoError(t, err)
	coord.Flush()

	_, rc, err := coord.Get(context.Background(), "b", "k")
	require.NoError(t, err)

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.True(t, bytes.Equal(data, got), "round-trip over an etcd-fed topology")
}

// memPeers dials in-process stores for the coordinator test.
type memPeers struct {
	stores map[cluster.NodeID]*transport.MemStore
}

func (p memPeers) Peer(node cluster.Node) (clusterstore.Peer, error) {
	return clusterstore.LocalPeer{Store: p.stores[node.ID]}, nil
}
