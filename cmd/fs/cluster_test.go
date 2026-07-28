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
	"github.com/go-faster/fs/internal/cluster/objindex"
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

// etcdStartAttempts is how many times startTestEtcd will re-pick ports.
//
// Ports come from testFreeAddr, which binds, closes, and hands the address on —
// a race, and unavoidably so: etcd wants its URLs in its config before anything
// binds, so unlike a node listener there is no port-0 form of this. Retrying
// makes the window irrelevant rather than narrower.
const etcdStartAttempts = 3

// startTestEtcd runs an in-process etcd for the wiring test.
func startTestEtcd(t *testing.T) string {
	t.Helper()

	for attempt := range etcdStartAttempts {
		endpoint, ok := tryStartTestEtcd(t, attempt == etcdStartAttempts-1)
		if ok {
			return endpoint
		}
	}

	t.Fatal("etcd never started")

	return ""
}

// tryStartTestEtcd makes one attempt, reporting failure rather than failing the
// test until the last one — where the real error is more useful than "never
// started".
func tryStartTestEtcd(t *testing.T, last bool) (string, bool) {
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
	if err != nil {
		if last {
			require.NoError(t, err)
		}

		// Almost always "address already in use": something took one of the
		// ports between the probe and the start. Another set is all it needs.
		t.Logf("etcd start attempt failed, retrying with fresh ports: %v", err)

		return "", false
	}

	t.Cleanup(srv.Close)

	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		t.Fatal("etcd did not become ready")
	}

	return clientURL.String(), true
}

// testNodeAddr is what a node under test binds to.
//
// Port 0, so the kernel allocates one at bind time and nothing can take it in
// between. Picking a port first and binding it second is a race that CI loses
// often enough to matter: the runs are parallel and every one of them wants
// ephemeral ports. buildCluster resolves the advertised port from what it
// actually bound, so peers still find each other.
const testNodeAddr = "127.0.0.1:0"

// testFreeAddr picks a port, releases it, and returns the address.
//
// Racy by construction, and only usable where the port must be known before
// anything binds it: embedded etcd's URLs, which its config wants up front, and
// the deliberately-dead peer address a test dials to prove nothing answers.
// Anything that binds a node should use testNodeAddr instead.
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
		addr := testNodeAddr

		cfg := validClusterConfig()
		cfg.Cluster.NodeID = "n" + strconv.Itoa(i)
		cfg.Cluster.Rack = "r" + strconv.Itoa(i)
		cfg.Cluster.Addr = addr
		cfg.Cluster.AdvertiseAddr = addr
		cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-wiring", TTL: 2 * time.Second}
		cfg.Cluster.Disks = []ClusterDiskConfig{
			{ID: "d0", Path: filepath.Join(t.TempDir(), "d0")},
			{ID: "d1", Path: filepath.Join(t.TempDir(), "d1"), Weight: diskWeight(2)},
		}
		cfg.Storage.Fsync = "none"
		require.NoError(t, cfg.Validate())

		rt, err := buildCluster(t.Context(), lg, cfg, t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.close() })

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

	listed, err := reader.ListObjects(t.Context(), &fs.ListObjectsRequest{Bucket: "b", Prefix: "dir/"})
	require.NoError(t, err)
	require.Len(t, listed.Objects, 1)
	assert.Equal(t, "dir/obj.bin", listed.Objects[0].Key)

	// Drain the writer's async remainder before deleting through a different
	// node. A write acks at quorum and extends its sidecar to the remaining
	// target afterward; the guard against a delete outrunning that extension —
	// and being undone by it — is per-coordinator and in-process, so it does
	// not apply when the delete arrives through another node. That race is the
	// cross-node linearizability gap the design records, not something this
	// test is here to exercise.
	nodes[0].coord.Flush()

	require.NoError(t, reader.DeleteObject(t.Context(), "b", "dir/obj.bin"))
	require.NoError(t, reader.DeleteBucket(t.Context(), "b"))
}

// diskWeight is a config weight as a pointer, which is how an explicit zero
// stays distinguishable from an omitted key.
func diskWeight(w float64) *float64 { return &w }

// TestClusterRegistersTheAdvertisedAddressFromEnv pins the one thing a node's
// registry record must carry: the address peers dial it on.
//
// FS_CLUSTER_ADVERTISE_ADDR exists so one config can serve every instance —
// a pod's stable DNS name, a compose service name — and validation accepts it
// in place of the config value. Registering the config field instead meant such
// a node started, passed its health check, and registered an address of ""; the
// failure surfaced only on the peers, as "node X has no address" against every
// write.
func TestClusterRegistersTheAdvertisedAddressFromEnv(t *testing.T) {
	endpoint := startTestEtcd(t)

	// A name rather than an address, which is what this override is for: the
	// value peers dial need not be anything this node could bind.
	const advertised = "node.svc.cluster.local:7080"

	cfg := validClusterConfig()
	cfg.Cluster.NodeID = ""
	cfg.Cluster.AdvertiseAddr = ""
	cfg.Cluster.Addr = testNodeAddr
	cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-advertise", TTL: 2 * time.Second}
	cfg.Cluster.Disks = []ClusterDiskConfig{{ID: "d0", Path: filepath.Join(t.TempDir(), "d0")}}
	cfg.Storage.Fsync = "none"

	t.Setenv("FS_CLUSTER_NODE_ID", "env-node")
	t.Setenv("FS_CLUSTER_ADVERTISE_ADDR", advertised)

	// The config is valid with the identity coming only from the environment.
	require.NoError(t, cfg.Validate())

	rt, err := buildCluster(t.Context(), zaptest.NewLogger(t), cfg, t.TempDir())
	require.NoError(t, err)

	t.Cleanup(func() { _ = rt.close() })

	require.Eventually(t, func() bool {
		return rt.coord.Topology().DiskCount() == 1
	}, 15*time.Second, 20*time.Millisecond)

	nodes := rt.coord.Topology().Nodes
	require.Len(t, nodes, 1)
	assert.EqualValues(t, "env-node", nodes[0].ID)
	assert.Equal(t, advertised, nodes[0].Addr,
		"peers must have something to dial, and it is the environment's answer "+
			"rather than the port this node happened to bind")
}

// TestFailedBindReleasesTheObjectIndex: by the time the listener is bound, the
// runtime already owns an open pebble database. A failure that returned without
// closing it would leave the index open for the life of the process — and on
// Windows an open database cannot even be deleted, which is the whole reason
// every other failure path below the bind goes through rt.close().
//
// Asserted by reopening it: pebble takes a lock on its directory, so a second
// Open succeeds only if the first was really closed.
func TestFailedBindReleasesTheObjectIndex(t *testing.T) {
	// Hold the port so the node's bind cannot succeed.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	root := t.TempDir()

	cfg := validClusterConfig()
	cfg.Cluster.Addr = held.Addr().String()
	cfg.Cluster.AdvertiseAddr = held.Addr().String()
	cfg.Cluster.Disks = []ClusterDiskConfig{{ID: "d0", Path: filepath.Join(root, "d0")}}
	cfg.Storage.Fsync = "none"

	_, err = buildCluster(t.Context(), zaptest.NewLogger(t), cfg, root)
	require.ErrorContains(t, err, "bind cluster listener")

	reopened, err := objindex.Open(objindex.DefaultDir(root))
	require.NoError(t, err, "the index was left open by the failed build")
	require.NoError(t, reopened.Close())
}

// TestAdvertisedPortZeroBecomesTheBoundPort: a node told to advertise port 0
// advertises what it actually bound.
//
// Binding is what allocates the port, so nothing before it can know the answer,
// and a registration carrying ":0" is one no peer can dial. Anything else is
// passed through untouched — an advertised address is often a name this node
// could not bind at all.
func TestAdvertisedPortZeroBecomesTheBoundPort(t *testing.T) {
	bound, err := net.ResolveTCPAddr("tcp", "10.0.0.7:41234")
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1:41234", resolveAdvertisePort("127.0.0.1:0", bound))
	assert.Equal(t, "node.svc:7080", resolveAdvertisePort("node.svc:7080", bound))
	assert.Equal(t, "node.svc", resolveAdvertisePort("node.svc", bound),
		"an address with no port at all is not this function's business")
	assert.Empty(t, resolveAdvertisePort("", bound))
}
