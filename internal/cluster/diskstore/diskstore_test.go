package diskstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
	"github.com/go-faster/fs/internal/cluster/transport"
	"github.com/go-faster/fs/storagefs"
)

func newStore(t *testing.T, disks ...cluster.DiskID) *diskstore.Store {
	t.Helper()

	roots := make(map[cluster.DiskID]string, len(disks))
	for _, d := range disks {
		roots[d] = filepath.Join(t.TempDir(), string(d))
	}

	s, err := diskstore.New(roots)
	require.NoError(t, err)

	return s
}

func put(t *testing.T, s transport.Store, disk cluster.DiskID, name string, data []byte) {
	t.Helper()

	w, err := s.Create(t.Context(), disk, name)
	require.NoError(t, err)

	_, err = w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())
}

// read returns a fragment's content from disk "d0".
func read(t *testing.T, s transport.Store, name string) []byte {
	t.Helper()

	rc, size, err := s.Open(t.Context(), "d0", name)
	require.NoError(t, err)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, int64(len(data)), size)

	return data
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return b
}

func TestRoundTrip(t *testing.T) {
	s := newStore(t, "d0", "d1")

	for _, name := range []string{
		"plain",
		"obj/2b4c/0011223344556677.f0",
		"nested/deep/path/frag",
		"with space/имя файла",
	} {
		for _, n := range []int{0, 1, 4097} {
			data := randBytes(n)
			put(t, s, "d0", name, data)

			assert.True(t, bytes.Equal(data, read(t, s, name)), "%s n=%d", name, n)

			size, err := s.Stat(t.Context(), "d0", name)
			require.NoError(t, err)
			assert.Equal(t, int64(n), size)
		}

		// Per-disk namespaces are isolated.
		_, err := s.Stat(t.Context(), "d1", name)
		require.ErrorIs(t, err, transport.ErrNotFound, "%s must not exist on d1", name)

		require.NoError(t, s.Delete(t.Context(), "d0", name))
		require.ErrorIs(t, s.Delete(t.Context(), "d0", name), transport.ErrNotFound)

		_, _, err = s.Open(t.Context(), "d0", name)
		require.ErrorIs(t, err, transport.ErrNotFound)
	}
}

func TestUnknownDiskAndInvalidNames(t *testing.T) {
	s := newStore(t, "d0")

	_, err := s.Create(t.Context(), "nope", "frag")
	require.Error(t, err)

	for _, name := range []string{"", ".", "..", "../escape", "a/../../b", "/abs", "a//b", "a/./b"} {
		_, err := s.Create(t.Context(), "d0", name)
		require.Error(t, err, "name %q must be rejected", name)

		_, _, err = s.Open(t.Context(), "d0", name)
		require.Error(t, err, "name %q must be rejected", name)
	}
}

func TestAtomicVisibility(t *testing.T) {
	s := newStore(t, "d0")

	w, err := s.Create(t.Context(), "d0", "obj/x/frag")
	require.NoError(t, err)

	_, err = w.Write([]byte("partial"))
	require.NoError(t, err)

	// Not closed: invisible to reads, stats and listings.
	_, _, err = s.Open(t.Context(), "d0", "obj/x/frag")
	require.ErrorIs(t, err, transport.ErrNotFound, "in-flight write must be invisible")

	names, err := s.List(t.Context(), "d0", "")
	require.NoError(t, err)
	assert.Empty(t, names, "temp files must not be listed")

	require.NoError(t, w.Close())
	assert.Equal(t, []byte("partial"), read(t, s, "obj/x/frag"))
}

func TestOverwriteReplaces(t *testing.T) {
	s := newStore(t, "d0")

	put(t, s, "d0", "frag", []byte("old content, longer"))
	put(t, s, "d0", "frag", []byte("new"))
	assert.Equal(t, []byte("new"), read(t, s, "frag"))
}

func TestDeletePrunesEmptyDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d0")

	s, err := diskstore.New(map[cluster.DiskID]string{"d0": root})
	require.NoError(t, err)

	put(t, s, "d0", "obj/aa/g1.f0", []byte("x"))
	put(t, s, "d0", "obj/aa/g1.f1", []byte("y"))

	// Deleting one fragment keeps the shared directory.
	require.NoError(t, s.Delete(t.Context(), "d0", "obj/aa/g1.f0"))

	_, err = os.Stat(filepath.Join(root, "obj", "aa"))
	require.NoError(t, err, "shared dir must survive")

	// Deleting the last fragment prunes the empty namespace up to the root.
	require.NoError(t, s.Delete(t.Context(), "d0", "obj/aa/g1.f1"))

	_, err = os.Stat(filepath.Join(root, "obj"))
	require.ErrorIs(t, err, os.ErrNotExist, "empty namespace dirs must be pruned")

	_, err = os.Stat(root)
	require.NoError(t, err, "the disk root must survive")
}

func TestDirectoryIsNotAFragment(t *testing.T) {
	s := newStore(t, "d0")
	put(t, s, "d0", "obj/aa/frag", []byte("x"))

	_, err := s.Stat(t.Context(), "d0", "obj/aa")
	require.ErrorIs(t, err, transport.ErrNotFound)
	require.ErrorIs(t, s.Delete(t.Context(), "d0", "obj/aa"), transport.ErrNotFound)
}

func TestList(t *testing.T) {
	s := newStore(t, "d0", "d1")

	put(t, s, "d0", "obj/bb/g1.f1", []byte("1"))
	put(t, s, "d0", "obj/aa/g1.f0", []byte("0"))
	put(t, s, "d0", "obj/aa/meta", []byte("m"))
	put(t, s, "d0", "other/frag", []byte("o"))
	put(t, s, "d1", "obj/aa/g9.f9", []byte("9"))

	names, err := s.List(t.Context(), "d0", "obj/")
	require.NoError(t, err)
	assert.Equal(t, []string{"obj/aa/g1.f0", "obj/aa/meta", "obj/bb/g1.f1"}, names, "sorted, prefix-filtered")

	all, err := s.List(t.Context(), "d0", "")
	require.NoError(t, err)
	assert.Len(t, all, 4)

	none, err := s.List(t.Context(), "d0", "missing/")
	require.NoError(t, err)
	assert.Empty(t, none)

	_, err = s.List(t.Context(), "nope", "")
	require.Error(t, err)
}

func TestSyncFileDirPolicy(t *testing.T) {
	// Exercise the fsync paths; durability itself is not assertable in a test.
	s, err := diskstore.New(
		map[cluster.DiskID]string{"d0": filepath.Join(t.TempDir(), "d0")},
		diskstore.WithSyncPolicy(storagefs.SyncFileDir),
	)
	require.NoError(t, err)

	data := randBytes(9000)
	put(t, s, "d0", "obj/aa/frag", data)
	assert.True(t, bytes.Equal(data, read(t, s, "obj/aa/frag")))
	require.NoError(t, s.Delete(t.Context(), "d0", "obj/aa/frag"))
}

func TestTransportOverDiskStore(t *testing.T) {
	secret := transport.Secret(randBytes(32))
	store := newStore(t, "d0")
	srv := httptest.NewServer(transport.NewServer(store, secret))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, secret, "n1", nil)
	require.NoError(t, err)

	data := randBytes(300_000)
	name := "obj/aa/g1.f0"

	require.NoError(t, client.Put(t.Context(), "d0", name, int64(len(data)), bytes.NewReader(data)))

	rc, size, err := client.Get(t.Context(), "d0", name)
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), size)

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.True(t, bytes.Equal(data, got), "digest-verified round-trip over HTTP")

	require.NoError(t, client.Delete(t.Context(), "d0", name))

	_, err = client.Stat(t.Context(), "d0", name)
	require.ErrorIs(t, err, transport.ErrNotFound)
}

// TestClusterstoreOverDiskStore runs the coordinator against a 3-node HTTP
// cluster whose fragments live on real disks: the full production data path
// minus etcd.
func TestClusterstoreOverDiskStore(t *testing.T) {
	secret := transport.Secret(randBytes(32))
	topo := &cluster.Topology{Epoch: 1}
	stores := make(map[cluster.NodeID]*diskstore.Store)

	for i := range 3 {
		id := cluster.NodeID("n" + strconv.Itoa(i))
		store := newStore(t, "d0", "d1")
		srv := httptest.NewServer(transport.NewServer(store, secret))
		t.Cleanup(srv.Close)

		stores[id] = store
		topo.Nodes = append(topo.Nodes, cluster.Node{
			ID:   id,
			Addr: srv.Listener.Addr().String(),
			Rack: "r" + strconv.Itoa(i),
			Disks: []cluster.Disk{
				{ID: "d0", Weight: 1},
				{ID: "d1", Weight: 1},
			},
		})
	}

	coord, err := clusterstore.New(clusterstore.Config{
		Topology: clusterstore.StaticTopology{T: topo},
		Peers:    clusterstore.NewHTTPPeers("n0", stores["n0"], secret, nil),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = coord.Close() })

	data := randBytes(200_000)

	_, err = coord.Put(context.Background(), &clusterstore.PutRequest{
		Bucket: "b", Key: "видео/clip 01.mp4", Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	require.NoError(t, err)
	coord.Flush()

	_, rc, err := coord.Get(context.Background(), "b", "видео/clip 01.mp4")
	require.NoError(t, err)

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.True(t, bytes.Equal(data, got))

	require.NoError(t, coord.Delete(context.Background(), "b", "видео/clip 01.mp4"))

	for id, store := range stores {
		for _, disk := range []cluster.DiskID{"d0", "d1"} {
			names, err := store.List(context.Background(), disk, "")
			require.NoError(t, err)
			assert.Empty(t, names, "node %s disk %s must be empty after delete", id, disk)
		}
	}
}
