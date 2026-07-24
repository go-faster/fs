package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminapi"
	"github.com/go-faster/fs/internal/adminhandler"
)

// newTestAdminServer builds the same handler chain runAdminServer serves (UI
// middleware + bearer guard + ogen server) around an httptest server.
func newTestAdminServer(t *testing.T, token string) (*httptest.Server, *auth.Manager) {
	t.Helper()

	mgr, err := auth.NewManager(auth.Config{Keys: []auth.Key{
		{AccessKey: "AKIACONFIG", SecretKey: "config-secret", Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}}},
	}}, "")
	require.NoError(t, err)

	handler := adminhandler.NewAdminAPI(adminhandler.Options{Manager: mgr, AuthEnabled: true})

	s, err := adminapi.NewServer(handler)
	require.NoError(t, err)

	srv := httptest.NewServer(adminhandler.UIMiddleware()(bearerAuth(token, s)))
	t.Cleanup(srv.Close)

	return srv, mgr
}

func TestAdminServer_RequiresToken(t *testing.T) {
	srv, _ := newTestAdminServer(t, "s3cr3t")

	// No token: 401.
	resp, err := http.Get(srv.URL + "/api/v1/info")
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Wrong token: 401.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1/info", http.NoBody)
	req.Header.Set("Authorization", "Bearer wrong")

	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp2.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
}

func TestAdminServer_ServesUIWithoutToken(t *testing.T) {
	srv, _ := newTestAdminServer(t, "s3cr3t")

	// Non-API paths (the SPA) load without a token.
	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
}

func TestAdminServer_CRUDViaClient(t *testing.T) {
	token := "s3cr3t"
	srv, mgr := newTestAdminServer(t, token)

	client, err := adminapi.NewClient(srv.URL, adminapi.WithClient(&http.Client{
		Transport: bearerTransport{token: token, base: http.DefaultTransport},
	}))
	require.NoError(t, err)

	ctx := context.Background()

	// Info.
	info, err := client.GetInfo(ctx)
	require.NoError(t, err)
	assert.True(t, info.AuthEnabled)

	// List: only the config key.
	list, err := client.ListAccessKeys(ctx)
	require.NoError(t, err)
	require.Len(t, list.Keys, 1)

	// Create.
	created, err := client.CreateAccessKey(ctx, &adminapi.CreateAccessKeyRequest{
		Grants: []adminapi.Grant{{Bucket: "uploads-*", Permission: adminapi.PermissionWrite}},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, created.AccessKey)
	assert.NotEmpty(t, created.SecretKey)

	// The created key authenticates against the shared store.
	secret, ok := mgr.Store().Secret(created.AccessKey)
	require.True(t, ok)
	assert.Equal(t, created.SecretKey, secret)

	// List now has two.
	list, err = client.ListAccessKeys(ctx)
	require.NoError(t, err)
	require.Len(t, list.Keys, 2)

	// Delete.
	require.NoError(t, client.DeleteAccessKey(ctx, adminapi.DeleteAccessKeyParams{AccessKey: created.AccessKey}))

	_, ok = mgr.Store().Secret(created.AccessKey)
	assert.False(t, ok)
}

// TestAdminServer_ReloadViaClient drives the reload endpoint the way an
// orchestrator does: read the loaded config revision, rewrite the config,
// reload through the API, and confirm the node reports the new revision — no
// SIGHUP, no shelling in.
func TestAdminServer_ReloadViaClient(t *testing.T) {
	token := "s3cr3t"
	mgr := managerWithKeyA(t)
	cfgPath := writeConfig(t, "revision: cfg-live\n"+configKeyB)
	rel := newReloader(zap.NewNop(), cfgPath, false, mgr, emptyServer(t))

	handler := adminhandler.NewAdminAPI(adminhandler.Options{
		Manager:        mgr,
		AuthEnabled:    true,
		Reloader:       rel,
		ConfigRevision: rel.CurrentRevision,
	})

	s, err := adminapi.NewServer(handler)
	require.NoError(t, err)

	srv := httptest.NewServer(adminhandler.UIMiddleware()(bearerAuth(token, s)))
	t.Cleanup(srv.Close)

	client, err := adminapi.NewClient(srv.URL, adminapi.WithClient(&http.Client{
		Transport: bearerTransport{token: token, base: http.DefaultTransport},
	}))
	require.NoError(t, err)

	ctx := context.Background()

	// The node reports the revision it loaded at startup.
	info, err := client.GetInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, "cfg-live", info.ConfigRevision.Or(""))

	// Rewrite the config to a new revision and reload through the API.
	require.NoError(t, os.WriteFile(cfgPath, []byte("revision: cfg-next\n"+configKeyB), 0o600))

	res, err := client.ReloadConfig(ctx)
	require.NoError(t, err)
	assert.Contains(t, res.Reloaded, "credentials")
	assert.Equal(t, "cfg-next", res.ConfigRevision.Or(""))

	// GetInfo now reports the advanced revision.
	info, err = client.GetInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, "cfg-next", info.ConfigRevision.Or(""))
}

// bearerTransport injects a bearer token on every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)

	return t.base.RoundTrip(r)
}
