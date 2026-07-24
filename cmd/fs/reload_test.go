package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

// managerWithKeyA builds a manager that already knows key A.
func managerWithKeyA(t *testing.T) *auth.Manager {
	t.Helper()

	mgr, err := auth.NewManager(auth.Config{Keys: []auth.Key{
		{AccessKey: "AKIAAAAAAAAAAAAAAAAA", SecretKey: "secret-a", Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}}},
	}}, "")
	require.NoError(t, err)

	return mgr
}

// writeConfig writes cfg to a temp file and returns its path.
func writeConfig(t *testing.T, cfg string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))

	return path
}

// emptyServer builds a server over in-memory storage.
func emptyServer(t *testing.T) *server.Server {
	t.Helper()

	srv, err := server.New(server.Config{Storage: storagemem.New()})
	require.NoError(t, err)

	return srv
}

const configKeyB = `
auth:
  keys:
    - access_key: AKIABBBBBBBBBBBBBBBB
      secret_key: secret-b
      grants:
        - bucket: "*"
          permission: admin
`

func TestReload_UpdatesCredentials(t *testing.T) {
	mgr := managerWithKeyA(t)
	store := mgr.Store()

	_, ok := store.Secret("AKIAAAAAAAAAAAAAAAAA")
	require.True(t, ok)

	// The config instead defines key B.
	rel := newReloader(zap.NewNop(), writeConfig(t, configKeyB), false, mgr, emptyServer(t))

	res, err := rel.Reload(context.Background())
	require.NoError(t, err)
	require.Contains(t, res.Reloaded, "credentials")

	// The store now knows B and no longer knows A.
	secret, ok := store.Secret("AKIABBBBBBBBBBBBBBBB")
	require.True(t, ok)
	require.Equal(t, "secret-b", secret)

	_, ok = store.Secret("AKIAAAAAAAAAAAAAAAAA")
	require.False(t, ok)
}

func TestReload_InvalidConfigKeepsCurrent(t *testing.T) {
	mgr := managerWithKeyA(t)

	rel := newReloader(zap.NewNop(), writeConfig(t, "not: [valid: yaml"), false, mgr, emptyServer(t))

	// A bad reload reports the failure and leaves the working credentials.
	_, err := rel.Reload(context.Background())
	require.Error(t, err)

	_, ok := mgr.Store().Secret("AKIAAAAAAAAAAAAAAAAA")
	require.True(t, ok)
}

func TestReload_PreservesRuntimeKeys(t *testing.T) {
	mgr := managerWithKeyA(t)

	// A key created at runtime through the admin API.
	created, err := mgr.Create(auth.CreateInput{Grants: []auth.Grant{{Pattern: "*", Permission: auth.Read}}})
	require.NoError(t, err)

	rel := newReloader(zap.NewNop(), writeConfig(t, configKeyB), false, mgr, emptyServer(t))

	_, err = rel.Reload(context.Background())
	require.NoError(t, err)

	// Config key rotated, but the runtime-created key survives.
	_, ok := mgr.Store().Secret("AKIABBBBBBBBBBBBBBBB")
	require.True(t, ok)

	_, ok = mgr.Store().Secret(created.AccessKey)
	require.True(t, ok, "runtime-created key must survive reload")
}

// TestReload_TracksRevision covers the config revision an orchestrator reads
// back to confirm a node loaded the config it rendered: the reloader reports
// the startup revision, and a reload advances it to the file's new value. The
// configs carry credentials so the reload itself succeeds.
func TestReload_TracksRevision(t *testing.T) {
	mgr := managerWithKeyA(t)
	cfgPath := writeConfig(t, "revision: cfg-1111\n"+configKeyB)

	rel := newReloader(zap.NewNop(), cfgPath, false, mgr, emptyServer(t))
	require.Equal(t, "cfg-1111", rel.CurrentRevision(), "startup revision")

	require.NoError(t, os.WriteFile(cfgPath, []byte("revision: cfg-2222\n"+configKeyB), 0o600))

	res, err := rel.Reload(context.Background())
	require.NoError(t, err)
	require.Equal(t, "cfg-2222", res.ConfigRevision)
	require.Equal(t, "cfg-2222", rel.CurrentRevision(), "revision advances on reload")
}
