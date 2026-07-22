package transport_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/transport"
)

func TestClientList(t *testing.T) {
	secret := make(transport.Secret, 32)
	_, _ = rand.Read(secret)

	store := transport.NewMemStore()
	srv := httptest.NewServer(transport.NewServer(store, secret))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "n1", nil)
	require.NoError(t, err)

	names := []string{
		"obj/aa/g1.f0",
		"obj/aa/meta",
		"obj/bb/имя с пробелами/meta",
		"bkt/cc/meta",
	}
	for _, name := range names {
		require.NoError(t, client.Put(t.Context(), "d0", name, 1, bytes.NewReader([]byte("x"))))
	}

	require.NoError(t, client.Put(t.Context(), "d1", "obj/aa/other-disk", 1, bytes.NewReader([]byte("y"))))

	got, err := client.List(t.Context(), "d0", "obj/")
	require.NoError(t, err)
	assert.Equal(t, []string{"obj/aa/g1.f0", "obj/aa/meta", "obj/bb/имя с пробелами/meta"}, got,
		"sorted, prefix-filtered, per-disk")

	all, err := client.List(t.Context(), "d0", "")
	require.NoError(t, err)
	assert.Len(t, all, len(names))

	empty, err := client.List(t.Context(), "d0", "missing/")
	require.NoError(t, err)
	assert.Empty(t, empty)

	// Unicode prefixes bind into the signed path and round-trip.
	uni, err := client.List(t.Context(), "d0", "obj/bb/имя")
	require.NoError(t, err)
	assert.Equal(t, []string{"obj/bb/имя с пробелами/meta"}, uni)
}

func TestListRequiresAuth(t *testing.T) {
	secret := make(transport.Secret, 32)
	_, _ = rand.Read(secret)

	srv := httptest.NewServer(transport.NewServer(transport.NewMemStore(), secret))
	t.Cleanup(srv.Close)

	wrong := make(transport.Secret, 32)
	_, _ = rand.Read(wrong)

	client, err := transport.NewClient(srv.URL, wrong, "n1", nil)
	require.NoError(t, err)

	_, err = client.List(t.Context(), "d0", "")
	require.ErrorIs(t, err, transport.ErrUnauthorized)
}

func TestMemStoreList(t *testing.T) {
	store := transport.NewMemStore()

	write := func(disk cluster.DiskID, name string) {
		w, err := store.Create(t.Context(), disk, name)
		require.NoError(t, err)

		_, err = io.WriteString(w, "x")
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}

	write("d0", "b/2")
	write("d0", "b/1")
	write("d0", "a/1")
	write("d1", "b/3")

	names, err := store.List(t.Context(), "d0", "b/")
	require.NoError(t, err)
	assert.Equal(t, []string{"b/1", "b/2"}, names)
}
