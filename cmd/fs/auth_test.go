package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/auth"
)

func TestBuildAuthStore_Disabled(t *testing.T) {
	t.Run("Flag", func(t *testing.T) {
		store, err := buildAuthStore(Config{}, true)
		require.NoError(t, err)
		require.Nil(t, store)
	})

	t.Run("ConfigDisabled", func(t *testing.T) {
		store, err := buildAuthStore(Config{Auth: AuthConfig{Disabled: true}}, false)
		require.NoError(t, err)
		require.Nil(t, store)
	})
}

func TestBuildAuthStore_NoCredentials(t *testing.T) {
	// Auth on by default with nothing configured is an error, never a silent
	// accept-anything.
	_, err := buildAuthStore(Config{}, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no credentials")
}

func TestBuildAuthStore_RootFromEnv(t *testing.T) {
	t.Setenv(envRootAccessKey, "AKIAROOT")
	t.Setenv(envRootSecretKey, "rootsecret")

	store, err := buildAuthStore(Config{}, false)
	require.NoError(t, err)
	require.NotNil(t, store)

	secret, ok := store.Secret("AKIAROOT")
	require.True(t, ok)
	require.Equal(t, "rootsecret", secret)
	require.True(t, store.Allow("AKIAROOT", "any-bucket", auth.ActionWrite))
}

func TestBuildAuthStore_RootMissingSecret(t *testing.T) {
	t.Setenv(envRootAccessKey, "AKIAROOT")

	_, err := buildAuthStore(Config{}, false)
	require.Error(t, err)
}

func TestBuildAuthStore_ConfigKeysAndGrants(t *testing.T) {
	cfg := Config{Auth: AuthConfig{
		Keys: []KeyConfig{{
			AccessKey: "AKIAUSER",
			SecretKey: "usersecret",
			Grants: []GrantConfig{
				{Bucket: "reports-*", Permission: "read"},
				{Bucket: "uploads", Permission: "write"},
			},
		}},
		PublicReadBuckets: []string{"public"},
	}}

	store, err := buildAuthStore(cfg, false)
	require.NoError(t, err)

	require.True(t, store.Allow("AKIAUSER", "reports-2026", auth.ActionRead))
	require.False(t, store.Allow("AKIAUSER", "reports-2026", auth.ActionWrite))
	require.True(t, store.Allow("AKIAUSER", "uploads", auth.ActionWrite))
	require.False(t, store.Allow("AKIAUSER", "other", auth.ActionRead))
	require.True(t, store.PublicRead("public"))
}

func TestParsePermission(t *testing.T) {
	for s, want := range map[string]auth.Permission{"read": auth.Read, "write": auth.Write, "admin": auth.Admin} {
		got, err := parsePermission(s)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	_, err := parsePermission("superuser")
	require.Error(t, err)
}
