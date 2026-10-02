package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/peer"
)

func TestValidateCluster(t *testing.T) {
	valid := func() Config {
		cfg := DefaultConfig()
		cfg.Cluster = ClusterConfig{NodeID: "n1", AdvertiseAddr: "10.0.0.1:7080", Secret: "0123456789abcdef"}

		return cfg
	}

	cfg := valid()
	require.NoError(t, cfg.Validate())

	cfg = DefaultConfig()
	require.NoError(t, cfg.Validate(), "no node id: cluster mode is off")

	cfg = valid()
	cfg.Cluster.AdvertiseAddr = ""
	require.ErrorContains(t, cfg.Validate(), "advertise_addr")

	cfg = valid()
	cfg.Cluster.Secret = "short"
	require.ErrorContains(t, cfg.Validate(), "secret")

	// An orchestrator injects per-node identity through the environment.
	t.Setenv("FS_CLUSTER_NODE_ID", "pod-2")
	t.Setenv("FS_CLUSTER_SECRET", "env-secret-0123456789")

	cfg = valid()
	cfg.Cluster.Secret = ""
	require.NoError(t, cfg.Validate())
	assert.Equal(t, "pod-2", cfg.clusterNodeID())
}

func TestReadRoles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.yaml")

	require.NoError(t, os.WriteFile(path, []byte(`
widths: [3, 6]
partitions: 64
members:
  - {id: a, zone: z1, rack: r1, capacity: 4TB}
  - {id: gw, capacity: "0"}
`), 0o600))

	req, err := readRoles(path)
	require.NoError(t, err)
	assert.Equal(t, []int{3, 6}, req.Widths)
	assert.Equal(t, 64, req.Partitions.Or(0))
	require.Len(t, req.Members, 2)
	assert.Equal(t, uint64(4_000_000_000_000), req.Members[0].Capacity)
	assert.Equal(t, "z1", req.Members[0].Zone.Or(""))
	assert.Zero(t, req.Members[1].Capacity)

	require.NoError(t, os.WriteFile(path, []byte("members: [{id: a, capcity: 1TB}]"), 0o600))
	_, err = readRoles(path)
	require.Error(t, err, "a misspelled field must not silently become a gateway")

	require.NoError(t, os.WriteFile(path, []byte("members: [{id: a, capacity: lots}]"), 0o600))
	_, err = readRoles(path)
	require.Error(t, err)
}

// TestLayoutViaAdminAPI drives the cluster endpoints the way `fs layout` does.
func TestLayoutViaAdminAPI(t *testing.T) {
	const token = "s3cr3t"

	m, err := peer.New(peer.Config{
		ID: "a", Addr: "127.0.0.1:1", Secret: peer.Secret("cluster-secret-0123456789"), Dir: t.TempDir(),
	})
	require.NoError(t, err)

	s, err := adminapi.NewServer(adminhandler.NewAdminAPI(adminhandler.Options{Cluster: m}))
	require.NoError(t, err)

	srv := httptest.NewServer(bearerAuth(token, s))
	t.Cleanup(srv.Close)

	client, err := adminapi.NewClient(srv.URL, adminapi.WithClient(&http.Client{
		Transport: bearerTransport{token: token, base: http.DefaultTransport},
	}))
	require.NoError(t, err)

	ctx := context.Background()

	_, err = client.GetLayout(ctx)
	require.Error(t, err, "no layout yet")

	roles := &adminapi.ApplyLayoutRequest{Partitions: adminapi.NewOptInt(16)}
	for _, id := range []string{"a", "b", "c"} {
		roles.Members = append(roles.Members, adminapi.LayoutRole{
			ID: id, Zone: adminapi.NewOptString("z-" + id), Capacity: 1 << 40,
		})
	}

	preview, err := client.ApplyLayout(ctx, roles, adminapi.ApplyLayoutParams{DryRun: adminapi.NewOptBool(true)})
	require.NoError(t, err)
	assert.False(t, preview.Applied)
	assert.Nil(t, m.Layout())

	change, err := client.ApplyLayout(ctx, roles, adminapi.ApplyLayoutParams{})
	require.NoError(t, err)
	assert.True(t, change.Applied)

	l, err := client.GetLayout(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), l.Version)
	assert.Equal(t, 16, l.Partitions)
	require.Len(t, l.Spread, 1)
	assert.Equal(t, 1, l.Spread[0].MaxPerZone, "three zones, three replicas")

	for _, member := range l.Members {
		assert.Equal(t, 16, member.Slots)
	}

	roles.Members = roles.Members[:2]
	_, err = client.ApplyLayout(ctx, roles, adminapi.ApplyLayoutParams{})
	require.Error(t, err, "two members cannot hold three replicas")

	nodes, err := client.ListClusterNodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes.Nodes, 1)
	assert.True(t, nodes.Nodes[0].Self)
	assert.Equal(t, uint64(1), nodes.Nodes[0].LayoutVersion.Or(0))

	// And the command itself, against the same server.
	var out bytes.Buffer

	cmd := Layout()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", "--admin-addr", srv.URL, "--token", token})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Version 1, 16 partitions")
}

func TestClusterEndpointsWhenOff(t *testing.T) {
	s, err := adminapi.NewServer(adminhandler.NewAdminAPI(adminhandler.Options{}))
	require.NoError(t, err)

	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/cluster/nodes")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}
