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

	"github.com/go-faster/fs/internal/cluster/etcd"
)

// TestSchemaGateAndMigrate covers the startup schema-compatibility gate and the
// `fs cluster migrate` reporting, against a real embedded etcd.
func TestSchemaGateAndMigrate(t *testing.T) {
	endpoint := startTestEtcd(t)
	lg := zaptest.NewLogger(t)

	newCfg := func(prefix string) Config {
		addr := testFreeAddr(t)

		cfg := validClusterConfig()
		cfg.Cluster.Addr = addr
		cfg.Cluster.AdvertiseAddr = addr
		cfg.Cluster.Etcd = EtcdConfig{Endpoints: []string{endpoint}, Prefix: prefix, TTL: 2 * time.Second}
		cfg.Storage.Fsync = "none"
		cfg.Storage.Root = t.TempDir()

		return cfg
	}

	client, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	t.Run("founds and gates", func(t *testing.T) {
		cfg := newCfg("/fs-schema-ok")

		rt, err := buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.close() })

		// The founding node stamped the current schema version.
		v, ok, err := etcd.LoadSchemaVersion(t.Context(), client, etcd.Config{Prefix: "/fs-schema-ok"})
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, etcd.SchemaVersion, v)

		// migrate reports up to date.
		var out bytes.Buffer
		require.NoError(t, runMigrate(t.Context(), &out, cfg, true))
		assert.Contains(t, out.String(), "up to date")
	})

	t.Run("refuses a too-new cluster", func(t *testing.T) {
		cfg := newCfg("/fs-schema-toonew")

		// Pre-stamp a schema version newer than this binary implements.
		_, err := client.Put(t.Context(), "/fs-schema-toonew/meta/schema-version", strconv.Itoa(etcd.SchemaVersion+1))
		require.NoError(t, err)

		_, err = buildCluster(t.Context(), lg, cfg, cfg.Storage.Root)
		require.ErrorIs(t, err, etcd.ErrSchemaTooNew)
	})
}
