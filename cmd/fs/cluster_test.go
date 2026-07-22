package main

import (
	"bytes"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/server/v3/embed"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
)

func validClusterConfig() Config {
	cfg := DefaultConfig()
	cfg.Storage.Type = StorageTypeCluster
	cfg.Cluster = ClusterConfig{
		NodeID:        "n0",
		Rack:          "r0",
		AdvertiseAddr: "127.0.0.1:7080",
		Secret:        "0123456789abcdef0123456789abcdef",
		Scheme:        "rf2.5",
		Etcd:          EtcdConfig{Endpoints: []string{"http://127.0.0.1:2379"}},
	}

	return cfg
}

func TestClusterConfigValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*Config)
		wantErr string
	}{
		"valid":             {mutate: func(*Config) {}},
		"missing node id":   {mutate: func(c *Config) { c.Cluster.NodeID = "" }, wantErr: "node_id"},
		"missing advertise": {mutate: func(c *Config) { c.Cluster.AdvertiseAddr = "" }, wantErr: "advertise_addr"},
		"short secret":      {mutate: func(c *Config) { c.Cluster.Secret = "short" }, wantErr: "secret"},
		"bad scheme":        {mutate: func(c *Config) { c.Cluster.Scheme = "rf9" }, wantErr: "scheme"},
		"no etcd endpoints": {mutate: func(c *Config) { c.Cluster.Etcd.Endpoints = nil }, wantErr: "endpoints"},
		"sub-second ttl":    {mutate: func(c *Config) { c.Cluster.Etcd.TTL = 100 * time.Millisecond }, wantErr: "ttl"},
		"disk without path": {mutate: func(c *Config) { c.Cluster.Disks = []ClusterDiskConfig{{ID: "d0"}} }, wantErr: "path"},
		"duplicate disk ids": {
			mutate: func(c *Config) {
				c.Cluster.Disks = []ClusterDiskConfig{{ID: "d0", Path: "/a"}, {ID: "d0", Path: "/b"}}
			},
			wantErr: "duplicate",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validClusterConfig()
			tc.mutate(&cfg)

			err := cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// startTestEtcd runs an in-process etcd for the wiring test.
func startTestEtcd(t *testing.T) string {
	t.Helper()

	cfg := embed.NewConfig()
	cfg.Dir = t.TempDir()
	cfg.LogLevel = "error"

	clientURL := url.URL{Scheme: "http", Host: testFreeAddr(t)}
	peerURL := url.URL{Scheme: "http", Host: testFreeAddr(t)}
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

	return clientURL.String()
}

func testFreeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

// TestClusterWiring boots three full cluster nodes from config — disk stores,
// etcd registration, topology watch, peer listeners — and round-trips an
// object written on one node and read from another.
func TestClusterWiring(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	grp, grpCtx := errgroup.WithContext(t.Context())

	nodes := make([]*clusterRuntime, 3)

	for i := range nodes {
		addr := testFreeAddr(t)

		cfg := validClusterConfig()
		cfg.Cluster.NodeID = "n" + strconv.Itoa(i)
		cfg.Cluster.Rack = "r" + strconv.Itoa(i)
		cfg.Cluster.Addr = addr
		cfg.Cluster.AdvertiseAddr = addr
		cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-wiring", TTL: 2 * time.Second}
		cfg.Cluster.Disks = []ClusterDiskConfig{
			{ID: "d0", Path: filepath.Join(t.TempDir(), "d0")},
			{ID: "d1", Path: filepath.Join(t.TempDir(), "d1"), Weight: 2},
		}
		cfg.Storage.Fsync = "none"
		require.NoError(t, cfg.Validate())

		rt, err := buildCluster(t.Context(), lg, cfg, t.TempDir())
		require.NoError(t, err)

		nodes[i] = rt

		grp.Go(func() error { return rt.Serve(grpCtx) })
	}

	// Wait until every node's topology has converged on all three members.
	writer, reader := nodes[0].Storage, nodes[2].Storage

	require.Eventually(t, func() bool {
		return nodes[0].coord.Topology().DiskCount() == 6 && nodes[2].coord.Topology().DiskCount() == 6
	}, 15*time.Second, 20*time.Millisecond, "topology must converge")

	// Full S3-level round-trip across nodes.
	require.NoError(t, writer.CreateBucket(t.Context(), "b"))

	data := bytes.Repeat([]byte("cluster!"), 10_000)

	put, err := writer.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket: "b", Key: "dir/obj.bin", Reader: bytes.NewReader(data), Size: int64(len(data)),
	})
	require.NoError(t, err)

	got, err := reader.GetObject(t.Context(), "b", "dir/obj.bin")
	require.NoError(t, err)
	assert.Equal(t, put.ETag, got.ETag)

	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.NoError(t, got.Reader.Close())
	assert.True(t, bytes.Equal(data, body), "cross-node read through full wiring")

	listed, err := reader.ListObjects(t.Context(), "b", "dir/")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "dir/obj.bin", listed[0].Key)

	require.NoError(t, reader.DeleteObject(t.Context(), "b", "dir/obj.bin"))
	require.NoError(t, reader.DeleteBucket(t.Context(), "b"))
}
