package adminhandler

import (
	"context"
	"crypto/rand"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/sse"
)

func TestRotateEncryptionKeys(t *testing.T) {
	ctx := context.Background()

	open := func(kr *sse.Keyring) *engine.Engine {
		e, err := engine.Open(t.TempDir(), engine.Options{Keyring: kr, NoSync: true})
		require.NoError(t, err)
		t.Cleanup(func() { _ = e.Close() })

		return e
	}

	api := newTestAPI(t)

	_, err := api.RotateEncryptionKeys(ctx)
	requireStatus(t, err, http.StatusNotImplemented)

	api.opts.Engine = open(nil)
	_, err = api.RotateEncryptionKeys(ctx)
	requireStatus(t, err, http.StatusNotImplemented)

	k := make([]byte, sse.KeySize)
	_, err = rand.Read(k)
	require.NoError(t, err)

	mk, err := sse.NewMasterKey(k)
	require.NoError(t, err)

	kr, err := sse.NewKeyring(mk)
	require.NoError(t, err)

	api.opts.Engine = open(kr)
	res, err := api.RotateEncryptionKeys(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Remaining)
	assert.NotNil(t, res.Failed, "an empty list, not null")
}
