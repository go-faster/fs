package etcd_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/etcd"
)

func TestEnsureCompatible(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/schema-test", TTL: 2}

	// No version recorded yet.
	_, ok, err := etcd.LoadSchemaVersion(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.False(t, ok)

	// Founding node stamps its version.
	v, err := etcd.EnsureCompatible(t.Context(), client, cfg, 3)
	require.NoError(t, err)
	assert.Equal(t, 3, v)

	got, ok, err := etcd.LoadSchemaVersion(t.Context(), client, cfg)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 3, got)

	// A same-version node joins.
	v, err = etcd.EnsureCompatible(t.Context(), client, cfg, 3)
	require.NoError(t, err)
	assert.Equal(t, 3, v)

	// A NEWER binary joins a v3 cluster: allowed, and it does NOT raise the
	// recorded version (peers still at v3 must not break).
	v, err = etcd.EnsureCompatible(t.Context(), client, cfg, 5)
	require.NoError(t, err)
	assert.Equal(t, 3, v)

	got, _, err = etcd.LoadSchemaVersion(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.Equal(t, 3, got, "a newer binary must not bump the cluster schema on join")

	// A stale binary (older than the cluster) refuses to start.
	_, err = etcd.EnsureCompatible(t.Context(), client, cfg, 2)
	require.ErrorIs(t, err, etcd.ErrSchemaTooNew)
}

func TestEnsureCompatibleConcurrentFounders(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/schema-race", TTL: 2}

	// Many nodes race to found the cluster; exactly one version is recorded
	// and every EnsureCompatible agrees on it.
	const founders = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []int
	)

	for range founders {
		wg.Go(func() {
			v, err := etcd.EnsureCompatible(t.Context(), client, cfg, 1)
			assert.NoError(t, err)

			mu.Lock()

			results = append(results, v)
			mu.Unlock()
		})
	}

	wg.Wait()

	for _, v := range results {
		assert.Equal(t, 1, v)
	}
}

// fakeMigration is a scripted Migration for the migrator tests.
type fakeMigration struct {
	version int
	applied *[]int
	fail    bool
	mu      *sync.Mutex
}

func (m fakeMigration) Version() int        { return m.version }
func (m fakeMigration) Description() string { return "test migration" }
func (m fakeMigration) Apply(context.Context) error {
	if m.fail {
		return assert.AnError
	}

	m.mu.Lock()
	*m.applied = append(*m.applied, m.version)
	m.mu.Unlock()

	return nil
}

func TestRunMigrations(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/migrate-test", TTL: 2}

	// Cluster founded at v1.
	_, err := etcd.EnsureCompatible(t.Context(), client, cfg, 1)
	require.NoError(t, err)

	var (
		mu      sync.Mutex
		applied []int
	)

	migs := []etcd.Migration{
		fakeMigration{version: 3, applied: &applied, mu: &mu}, // Out of order on purpose.
		fakeMigration{version: 2, applied: &applied, mu: &mu},
	}

	// A binary that only implements v2 applies just v2, not v3.
	got, err := etcd.RunMigrations(t.Context(), client, cfg, 2, "runner-a", migs)
	require.NoError(t, err)
	assert.Equal(t, []int{2}, got)

	mu.Lock()
	assert.Equal(t, []int{2}, applied)
	mu.Unlock()

	v, _, err := etcd.LoadSchemaVersion(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.Equal(t, 2, v)

	// Re-run at v2: already applied, nothing happens (idempotent/resumable).
	got, err = etcd.RunMigrations(t.Context(), client, cfg, 2, "runner-a", migs)
	require.NoError(t, err)
	assert.Empty(t, got)

	// A v3 binary now applies the remaining migration (resume from v2).
	applied = nil

	got, err = etcd.RunMigrations(t.Context(), client, cfg, 3, "runner-a", migs)
	require.NoError(t, err)
	assert.Equal(t, []int{3}, got)

	v, _, err = etcd.LoadSchemaVersion(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.Equal(t, 3, v)
}

func TestRunMigrationsStopsOnError(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/migrate-err", TTL: 2}

	_, err := etcd.EnsureCompatible(t.Context(), client, cfg, 1)
	require.NoError(t, err)

	var (
		mu      sync.Mutex
		applied []int
	)

	migs := []etcd.Migration{
		fakeMigration{version: 2, applied: &applied, mu: &mu},
		fakeMigration{version: 3, fail: true, applied: &applied, mu: &mu},
		fakeMigration{version: 4, applied: &applied, mu: &mu},
	}

	got, err := etcd.RunMigrations(t.Context(), client, cfg, 4, "runner", migs)
	require.Error(t, err)
	assert.Equal(t, []int{2}, got, "applies up to the failing migration, then stops")

	// The recorded version is v2: v3 failed, v4 never ran.
	v, _, err := etcd.LoadSchemaVersion(t.Context(), client, cfg)
	require.NoError(t, err)
	assert.Equal(t, 2, v)
}

func TestRunMigrationsRefusesTooNew(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/migrate-toonew", TTL: 2}

	_, err := etcd.EnsureCompatible(t.Context(), client, cfg, 5)
	require.NoError(t, err)

	_, err = etcd.RunMigrations(t.Context(), client, cfg, 2, "runner", nil)
	require.ErrorIs(t, err, etcd.ErrSchemaTooNew)
}
