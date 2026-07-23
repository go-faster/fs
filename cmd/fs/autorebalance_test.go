package main

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// TestRebalancePolicyTick exercises the policy decisions deterministically:
// hysteresis, an occupied cluster-wide slot, cooldown, and manual-wins on an
// operator pause.
func TestRebalancePolicyTick(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	addr := testFreeAddr(t)

	cfg := validClusterConfig()
	cfg.Cluster.Addr = addr
	cfg.Cluster.AdvertiseAddr = addr
	cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-policy", TTL: 2 * time.Second}
	cfg.Storage.Fsync = "none"
	cfg.Storage.Root = t.TempDir()

	rt, err := buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
	require.NoError(t, err)

	grp, grpCtx := errgroup.WithContext(t.Context())
	grp.Go(func() error { return rt.Serve(grpCtx) })

	ctl := rt.rebalance
	now := time.Now()

	p := &rebalancePolicy{
		lg:         lg,
		ctl:        ctl,
		settle:     10 * time.Second,
		cooldown:   time.Minute,
		appliedSig: "something-else", // The membership reads as "needs a walk".
	}

	// First tick arms the settle window; nothing starts.
	p.tick(t.Context(), now)
	assert.Equal(t, adminhandler.RebalanceIdle, ctl.Status(t.Context()).State)

	// Occupied slot: even past the settle window the policy stays out of the
	// way of a manual/foreign runner.
	client, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	external, err := etcd.CampaignRebalance(t.Context(), client, ctl.etcdCfg, "manual-runner")
	require.NoError(t, err)

	p.tick(t.Context(), now.Add(11*time.Second))
	assert.Equal(t, adminhandler.RebalanceIdle, ctl.Status(t.Context()).State)
	assert.True(t, p.lastAttempt.IsZero(), "an occupied slot must not consume the cooldown")

	// Manual wins: an operator pause is never overridden by the policy.
	require.NoError(t, ctl.Start(t.Context()))
	require.Eventually(t, func() bool {
		return ctl.Status(t.Context()).State == adminhandler.RebalanceWaiting
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, ctl.Pause(t.Context()))
	require.NoError(t, external.Close())

	p.tick(t.Context(), now.Add(22*time.Second))
	assert.Equal(t, adminhandler.RebalancePaused, ctl.Status(t.Context()).State, "policy must not resume an operator pause")

	// Operator resumes; the run completes and records the applied signature.
	require.NoError(t, ctl.Resume(t.Context()))
	require.Eventually(t, func() bool {
		return ctl.Status(t.Context()).State == adminhandler.RebalanceDone
	}, 30*time.Second, 50*time.Millisecond)

	sig := rt.coord.Topology().Signature()

	applied, ok, err := etcd.LoadRebalanceApplied(t.Context(), client, ctl.etcdCfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sig, applied)

	// With applied == current, the policy adopts it and stays quiet.
	p.tick(t.Context(), now.Add(40*time.Second))
	assert.Equal(t, sig, p.appliedSig)
	assert.True(t, p.lastAttempt.IsZero())
}

// TestAutoRebalanceConverges is the Phase 9 acceptance path: adding a node
// converges placement without operator action.
func TestAutoRebalanceConverges(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	grp, grpCtx := errgroup.WithContext(t.Context())

	auto := RebalanceConfig{Settle: time.Second, Cooldown: time.Second}

	nodeConfig := func(i int) Config {
		addr := testFreeAddr(t)

		cfg := validClusterConfig()
		cfg.Cluster.NodeID = "n" + strconv.Itoa(i)
		cfg.Cluster.Rack = "r" + strconv.Itoa(i)
		cfg.Cluster.Addr = addr
		cfg.Cluster.AdvertiseAddr = addr
		cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-auto", TTL: 2 * time.Second}
		cfg.Cluster.Rebalance = auto
		cfg.Storage.Fsync = "none"
		cfg.Storage.Root = t.TempDir()

		return cfg
	}

	boot := func(i int) *clusterRuntime {
		cfg := nodeConfig(i)

		rt, err := buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
		require.NoError(t, err)

		grp.Go(func() error { return rt.Serve(grpCtx) })
		grp.Go(func() error { rt.RunAutoRebalancer(grpCtx, auto); return nil })

		return rt
	}

	nodes := make([]*clusterRuntime, 3)
	for i := range nodes {
		nodes[i] = boot(i)
	}

	require.Eventually(t, func() bool {
		return nodes[0].coord.Topology().DiskCount() == 3
	}, 15*time.Second, 20*time.Millisecond, "topology must converge")

	require.NoError(t, nodes[0].Storage.CreateBucket(t.Context(), "b"))

	payload := make(map[string][]byte)

	for i := range 10 {
		key := "k" + strconv.Itoa(i)
		payload[key] = bytes.Repeat([]byte{byte(i)}, 4000)

		_, err := nodes[0].Storage.PutObject(t.Context(), &fs.PutObjectRequest{
			Bucket: "b", Key: key, Reader: bytes.NewReader(payload[key]), Size: int64(len(payload[key])),
		})
		require.NoError(t, err)
	}

	// Add a node; no operator action follows.
	added := boot(3)

	client, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	etcdCfg := etcd.Config{Prefix: "/fs-auto", TTL: 2}

	// Some node's policy triggers the walk and records the new signature.
	require.Eventually(t, func() bool {
		topo := added.coord.Topology()
		if topo.DiskCount() != 4 {
			return false
		}

		applied, ok, err := etcd.LoadRebalanceApplied(t.Context(), client, etcdCfg)

		return err == nil && ok && applied == topo.Signature()
	}, 90*time.Second, 200*time.Millisecond, "auto rebalance must converge after node add")

	// Everything reads back through the new node.
	for key, want := range payload {
		got, err := added.Storage.GetObject(t.Context(), "b", key)
		require.NoError(t, err, key)

		body := readAll(t, got.Reader)
		assert.True(t, bytes.Equal(want, body), "post-auto-rebalance read %s", key)
	}
}

// readAll drains and closes a reader.
func readAll(t *testing.T, rc interface {
	Read([]byte) (int, error)
	Close() error
},
) []byte {
	t.Helper()

	var buf bytes.Buffer

	_, err := buf.ReadFrom(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	return buf.Bytes()
}
