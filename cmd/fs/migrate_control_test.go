package main

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// testMigration is a schema migration that only records that it ran.
type testMigration struct {
	version int
	desc    string
	runs    atomic.Int64
	err     error
}

func (m *testMigration) Version() int        { return m.version }
func (m *testMigration) Description() string { return m.desc }

func (m *testMigration) Apply(context.Context) error {
	m.runs.Add(1)
	return m.err
}

// TestMigrateControllerAppliesPending drives the admin API's migration control
// against a real etcd: a cluster at the founding version, a binary that
// implements one version more, and the migration between them.
func TestMigrateControllerAppliesPending(t *testing.T) {
	const prefix = "/fs-migrate-control"

	endpoint := startTestEtcd(t)

	client, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	etcdCfg := etcd.Config{Prefix: prefix, TTL: 2}

	// Found the cluster at the current schema version, as a joining node does.
	_, err = etcd.EnsureCompatible(t.Context(), client, etcdCfg, etcd.SchemaVersion)
	require.NoError(t, err)

	next := etcd.SchemaVersion + 1
	mig := &testMigration{version: next, desc: "rewrite sidecars"}

	ctl := newMigrateController(client, etcdCfg, "admin-test", []etcd.Migration{mig})
	ctl.binaryVersion = next

	st, err := ctl.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, etcd.SchemaVersion, st.ClusterVersion)
	assert.Equal(t, next, st.BinaryVersion)
	require.Len(t, st.Pending, 1)
	assert.Equal(t, "rewrite sidecars", st.Pending[0].Description)

	st, err = ctl.Apply(t.Context())
	require.NoError(t, err)

	assert.Equal(t, int64(1), mig.runs.Load())
	assert.Equal(t, next, st.ClusterVersion, "the applied version is recorded in etcd")
	assert.Empty(t, st.Pending)
	assert.Equal(t, []int{next}, st.LastApplied)
	assert.False(t, st.Running)

	// Applying again is a no-op: nothing pending, so nothing campaigns or runs.
	st, err = ctl.Apply(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), mig.runs.Load())
	assert.Empty(t, st.Pending)

	// A binary older than the migrated cluster refuses to touch it.
	old := newMigrateController(client, etcdCfg, "admin-old", nil)
	old.binaryVersion = etcd.SchemaVersion

	_, err = old.Apply(t.Context())
	require.ErrorIs(t, err, adminhandler.ErrMigrationConflict)
	require.ErrorContains(t, err, "newer than this binary")
}

// TestMigrateControllerConflicts covers the states an apply refuses: one
// already running on this process, and a cluster no node has joined yet.
func TestMigrateControllerConflicts(t *testing.T) {
	endpoint := startTestEtcd(t)

	client, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	// Nothing recorded: no node has joined, so there is nothing to migrate.
	empty := newMigrateController(client, etcd.Config{Prefix: "/fs-migrate-empty", TTL: 2}, "admin-test", nil)

	st, err := empty.Status(t.Context())
	require.NoError(t, err)
	assert.Zero(t, st.ClusterVersion)
	assert.Empty(t, st.Pending)

	_, err = empty.Apply(t.Context())
	require.ErrorIs(t, err, adminhandler.ErrMigrationConflict)

	// One apply at a time per process.
	const prefix = "/fs-migrate-busy"

	etcdCfg := etcd.Config{Prefix: prefix, TTL: 2}

	_, err = etcd.EnsureCompatible(t.Context(), client, etcdCfg, etcd.SchemaVersion)
	require.NoError(t, err)

	next := etcd.SchemaVersion + 1

	busy := newMigrateController(client, etcdCfg, "admin-test", []etcd.Migration{&testMigration{version: next}})
	busy.binaryVersion = next

	require.True(t, busy.begin(), "claims the apply slot")

	_, err = busy.Apply(t.Context())
	require.ErrorIs(t, err, adminhandler.ErrMigrationConflict)

	st, err = busy.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, st.Running)

	// A failing migration leaves the slot free and the reason recorded.
	busy.finish(nil, nil)

	failing := newMigrateController(client, etcdCfg, "admin-test",
		[]etcd.Migration{&testMigration{version: next, err: assert.AnError}})
	failing.binaryVersion = next

	_, err = failing.Apply(t.Context())
	require.Error(t, err)
	assert.NotErrorIs(t, err, adminhandler.ErrMigrationConflict)

	st, err = failing.Status(t.Context())
	require.NoError(t, err)
	assert.False(t, st.Running)
	assert.NotEmpty(t, st.LastErr)

	// The cluster stays at the version it had: a failed migration is not
	// recorded as applied.
	v, ok, err := etcd.LoadSchemaVersion(t.Context(), client, etcdCfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, etcd.SchemaVersion, v, "schema version unchanged: "+strconv.Itoa(v))
}
