package adminhandler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminapi"
)

func newTestAPI(t *testing.T) *AdminAPI {
	t.Helper()

	m, err := auth.NewManager(auth.Config{
		Keys: []auth.Key{{
			AccessKey: "AKIACONFIG",
			SecretKey: "config-secret",
			Grants:    []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
		}},
	}, "")
	require.NoError(t, err)

	api := NewAdminAPI(Options{
		Manager:     m,
		Build:       BuildInfo{Version: "v1.2.3", Commit: "abcdef0"},
		AuthEnabled: true,
		StartTime:   time.Now().Add(-time.Minute),
	})

	return api
}

func TestGetInfo(t *testing.T) {
	api := newTestAPI(t)

	info, err := api.GetInfo(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "v1.2.3", info.Version)
	assert.Equal(t, "abcdef0", info.Commit)
	assert.True(t, info.AuthEnabled)
	assert.Greater(t, info.UptimeSeconds, 0.0)
	assert.NotEmpty(t, info.GoVersion)
}

func TestListAccessKeys(t *testing.T) {
	api := newTestAPI(t)

	list, err := api.ListAccessKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, list.Keys, 1)

	k := list.Keys[0]
	assert.Equal(t, "AKIACONFIG", k.AccessKey)
	assert.Equal(t, adminapi.SourceConfig, k.Source)
	require.Len(t, k.Grants, 1)
	assert.Equal(t, "*", k.Grants[0].Bucket)
	assert.Equal(t, adminapi.PermissionAdmin, k.Grants[0].Permission)
	assert.False(t, k.CreatedAt.Set, "config key has no creation time")
}

func TestCreateAccessKey(t *testing.T) {
	api := newTestAPI(t)

	created, err := api.CreateAccessKey(context.Background(), &adminapi.CreateAccessKeyRequest{
		Grants: []adminapi.Grant{{Bucket: "uploads-*", Permission: adminapi.PermissionWrite}},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, created.AccessKey)
	assert.NotEmpty(t, created.SecretKey)
	assert.False(t, created.CreatedAt.IsZero())

	// Now listed with a creation time and managed source.
	list, err := api.ListAccessKeys(context.Background())
	require.NoError(t, err)
	require.Len(t, list.Keys, 2)

	var managed *adminapi.AccessKey

	for i := range list.Keys {
		if list.Keys[i].Source == adminapi.SourceManaged {
			managed = &list.Keys[i]
		}
	}

	require.NotNil(t, managed)
	assert.Equal(t, created.AccessKey, managed.AccessKey)
	assert.True(t, managed.CreatedAt.Set)
}

func TestCreateAccessKeyValidation(t *testing.T) {
	api := newTestAPI(t)

	// Empty bucket pattern is rejected with 400.
	_, err := api.CreateAccessKey(context.Background(), &adminapi.CreateAccessKeyRequest{
		Grants: []adminapi.Grant{{Bucket: "", Permission: adminapi.PermissionRead}},
	})
	requireStatus(t, err, http.StatusBadRequest)
}

func TestCreateAccessKeyConflict(t *testing.T) {
	api := newTestAPI(t)

	_, err := api.CreateAccessKey(context.Background(), &adminapi.CreateAccessKeyRequest{
		AccessKey: adminapi.NewOptString("AKIACONFIG"),
		SecretKey: adminapi.NewOptString("x"),
		Grants:    []adminapi.Grant{{Bucket: "*", Permission: adminapi.PermissionRead}},
	})
	requireStatus(t, err, http.StatusConflict)
}

func TestDeleteAccessKey(t *testing.T) {
	api := newTestAPI(t)

	created, err := api.CreateAccessKey(context.Background(), &adminapi.CreateAccessKeyRequest{
		Grants: []adminapi.Grant{{Bucket: "*", Permission: adminapi.PermissionRead}},
	})
	require.NoError(t, err)

	require.NoError(t, api.DeleteAccessKey(context.Background(), adminapi.DeleteAccessKeyParams{AccessKey: created.AccessKey}))

	// Deleting a config key is forbidden.
	err = api.DeleteAccessKey(context.Background(), adminapi.DeleteAccessKeyParams{AccessKey: "AKIACONFIG"})
	requireStatus(t, err, http.StatusForbidden)

	// Deleting an unknown key is not found.
	err = api.DeleteAccessKey(context.Background(), adminapi.DeleteAccessKeyParams{AccessKey: "AKIAUNKNOWN"})
	requireStatus(t, err, http.StatusNotFound)
}

func requireStatus(t *testing.T, err error, want int) {
	t.Helper()
	require.Error(t, err)

	var sc *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &sc)
	assert.Equal(t, want, sc.StatusCode)
}
