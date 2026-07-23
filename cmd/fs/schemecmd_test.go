package main

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"
)

func TestClusterSchemeCommand(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	grp, grpCtx := errgroup.WithContext(t.Context())

	var (
		cliCfg Config
		first  *clusterRuntime
	)

	for i := range 3 {
		addr := testFreeAddr(t)

		cfg := validClusterConfig()
		cfg.Cluster.NodeID = "n" + strconv.Itoa(i)
		cfg.Cluster.Rack = "r" + strconv.Itoa(i)
		cfg.Cluster.Addr = addr
		cfg.Cluster.AdvertiseAddr = addr
		cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: "/fs-scheme-cmd", TTL: 2 * time.Second}
		cfg.Storage.Fsync = "none"
		cfg.Storage.Root = t.TempDir()

		rt, err := buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
		require.NoError(t, err)

		grp.Go(func() error { return rt.Serve(grpCtx) })

		if i == 0 {
			cliCfg = cfg
			first = rt
		}
	}

	require.Eventually(t, func() bool {
		return first.coord.Topology().DiskCount() == 3
	}, 15*time.Second, 20*time.Millisecond, "topology must converge")

	require.NoError(t, first.Storage.CreateBucket(t.Context(), "photos"))

	var out bytes.Buffer

	// Show: cluster default.
	require.NoError(t, runScheme(t.Context(), &out, cliCfg, "photos", "", false))
	assert.Contains(t, out.String(), "cluster default (rf2.5)")

	// Set rf3.
	out.Reset()
	require.NoError(t, runScheme(t.Context(), &out, cliCfg, "photos", "rf3", true))
	assert.Contains(t, out.String(), "bucket photos: rf3")

	// Unknown bucket and unparseable scheme fail.
	require.Error(t, runScheme(t.Context(), &out, cliCfg, "nope", "rf3", true))
	require.Error(t, runScheme(t.Context(), &out, cliCfg, "photos", "rf9", true))

	// "default" clears the override.
	out.Reset()
	require.NoError(t, runScheme(t.Context(), &out, cliCfg, "photos", "default", true))
	assert.Contains(t, out.String(), "cluster default (rf2.5)")
}
