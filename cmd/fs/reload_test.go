package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

func TestReload_UpdatesCredentials(t *testing.T) {
	// Start with a store that knows key A.
	store, err := auth.NewStore(auth.Config{Keys: []auth.Key{
		{AccessKey: "AKIAAAAAAAAAAAAAAAAA", SecretKey: "secret-a", Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}}},
	}})
	require.NoError(t, err)

	_, ok := store.Secret("AKIAAAAAAAAAAAAAAAAA")
	require.True(t, ok)

	// Write a config that instead defines key B.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
auth:
  keys:
    - access_key: AKIABBBBBBBBBBBBBBBB
      secret_key: secret-b
      grants:
        - bucket: "*"
          permission: admin
`), 0o600))

	srv, err := server.New(server.Config{Storage: storagemem.New()})
	require.NoError(t, err)

	reload(zap.NewNop(), cfgPath, false, store, srv)

	// The store now knows B and no longer knows A.
	secret, ok := store.Secret("AKIABBBBBBBBBBBBBBBB")
	require.True(t, ok)
	require.Equal(t, "secret-b", secret)

	_, ok = store.Secret("AKIAAAAAAAAAAAAAAAAA")
	require.False(t, ok)
}

func TestReload_InvalidConfigKeepsCurrent(t *testing.T) {
	store, err := auth.NewStore(auth.Config{Keys: []auth.Key{
		{AccessKey: "AKIAAAAAAAAAAAAAAAAA", SecretKey: "secret-a", Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}}},
	}})
	require.NoError(t, err)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("not: [valid: yaml"), 0o600))

	srv, err := server.New(server.Config{Storage: storagemem.New()})
	require.NoError(t, err)

	// A bad reload must leave the working credentials in place.
	reload(zap.NewNop(), cfgPath, false, store, srv)

	_, ok := store.Secret("AKIAAAAAAAAAAAAAAAAA")
	require.True(t, ok)
}
