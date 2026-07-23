package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
)

func TestFormatBytes(t *testing.T) {
	for want, n := range map[string]int64{
		"0 B":     0,
		"999 B":   999,
		"1.0 KiB": 1024,
		"1.5 MiB": 3 << 19,
		"2.0 GiB": 2 << 30,
	} {
		assert.Equal(t, want, formatBytes(n), "n=%d", n)
	}
}

func TestPrintRebalancePlan(t *testing.T) {
	var buf bytes.Buffer

	printRebalancePlan(&buf, &clusterstore.RebalancePlan{
		Objects:          10,
		MisplacedObjects: 3,
		MisplacedBytes:   4096,
		Nodes: map[cluster.NodeID]*clusterstore.NodePlan{
			"n1": {Objects: 2, Bytes: 3072},
			"n0": {Objects: 1, Bytes: 1024},
		},
	})

	out := buf.String()
	assert.Contains(t, out, "objects to move:   3")
	assert.Contains(t, out, "4.0 KiB")
	assert.Contains(t, out, "n0")
	assert.Contains(t, out, "n1")
	assert.Less(t, bytes.Index(buf.Bytes(), []byte("n0")), bytes.Index(buf.Bytes(), []byte("n1")), "nodes sorted")

	buf.Reset()
	printRebalancePlan(&buf, &clusterstore.RebalancePlan{Objects: 5, Nodes: map[cluster.NodeID]*clusterstore.NodePlan{}})
	assert.Contains(t, buf.String(), "nothing to move")
}

func TestClusterRebalanceFlagValidation(t *testing.T) {
	cmd := ClusterRebalance()
	cmd.SetArgs([]string{})
	require.ErrorContains(t, cmd.Execute(), "etcd.endpoints")
}

// TestClusterRebalanceEndToEnd boots three cluster nodes, writes objects, adds
// a fourth node and drives `fs cluster rebalance` (dry-run and live) as the
// disk-less CLI client against the real etcd and peer transport.
func TestClusterRebalanceEndToEnd(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	grp, grpCtx := errgroup.WithContext(t.Context())

	nodeConfig := func(i int) Config {
		addr := testFreeAddr(t)

		cfg := validClusterConfig()
		cfg.Cluster.NodeID = "n" + strconv.Itoa(i)
		cfg.Cluster.Rack = "r" + strconv.Itoa(i)
		cfg.Cluster.Addr = addr
		cfg.Cluster.AdvertiseAddr = addr
		cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-rebalance", TTL: 2 * time.Second}
		cfg.Cluster.Disks = []ClusterDiskConfig{{ID: "d0", Path: filepath.Join(t.TempDir(), "d0")}}
		cfg.Storage.Fsync = "none"

		return cfg
	}

	nodes := make([]*clusterRuntime, 3)

	for i := range nodes {
		rt, err := buildCluster(t.Context(), lg, nodeConfig(i), t.TempDir())
		require.NoError(t, err)

		nodes[i] = rt

		grp.Go(func() error { return rt.Serve(grpCtx) })
	}

	require.Eventually(t, func() bool {
		return nodes[0].coord.Topology().DiskCount() == 3
	}, 15*time.Second, 20*time.Millisecond, "initial topology must converge")

	require.NoError(t, nodes[0].Storage.CreateBucket(t.Context(), "b"))

	payload := make(map[string][]byte)

	for i := range 10 {
		key := "k" + strconv.Itoa(i)
		payload[key] = bytes.Repeat([]byte{byte(i)}, 4000+i)

		_, err := nodes[0].Storage.PutObject(t.Context(), &fs.PutObjectRequest{
			Bucket: "b", Key: key, Reader: bytes.NewReader(payload[key]), Size: int64(len(payload[key])),
		})
		require.NoError(t, err)
	}

	// Grow the cluster: some placements move to the new node.
	rt, err := buildCluster(t.Context(), lg, nodeConfig(3), t.TempDir())
	require.NoError(t, err)

	grp.Go(func() error { return rt.Serve(grpCtx) })

	cliCfg := nodeConfig(0) // etcd endpoints + secret are what runRebalance uses.

	// Dry run: the plan reports pending moves and changes nothing.
	var out bytes.Buffer

	require.NoError(t, runRebalance(t.Context(), &out, cliCfg, rebalanceParams{dryRun: true, concurrency: 2}))
	assert.Contains(t, out.String(), "objects examined:  10")
	assert.NotContains(t, out.String(), "objects to move:   0", "growth must leave something to move:\n%s", out.String())

	// Live run: elected, walked, converged, cursor cleared.
	out.Reset()
	require.NoError(t, runRebalance(t.Context(), &out, cliCfg, rebalanceParams{concurrency: 2, rateMiB: 512, verify: true}))
	assert.Contains(t, out.String(), "0 failed")

	// A second dry run confirms convergence.
	out.Reset()
	require.NoError(t, runRebalance(t.Context(), &out, cliCfg, rebalanceParams{dryRun: true}))
	assert.Contains(t, out.String(), "objects to move:   0", "post-rebalance plan must be empty:\n%s", out.String())

	// Every object still reads back intact from the new node.
	for key, want := range payload {
		got, err := rt.Storage.GetObject(t.Context(), "b", key)
		require.NoError(t, err, key)

		body, err := io.ReadAll(got.Reader)
		require.NoError(t, err, key)
		require.NoError(t, got.Reader.Close())
		assert.True(t, bytes.Equal(want, body), "post-rebalance read %s", key)
	}
}
