package clusterstore

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Checksum expectations, not crypto.
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// trackingStore wraps a MemStore and records the names it holds, so tests can
// assert cleanliness (no orphan fragments) without transport growing a List.
type trackingStore struct {
	*transport.MemStore

	mu    sync.Mutex
	names map[string]struct{}
}

func newTrackingStore() *trackingStore {
	return &trackingStore{MemStore: transport.NewMemStore(), names: make(map[string]struct{})}
}

type trackingWriter struct {
	io.WriteCloser

	s   *trackingStore
	key string
}

func (w trackingWriter) Close() error {
	if err := w.WriteCloser.Close(); err != nil {
		return err
	}

	w.s.mu.Lock()
	w.s.names[w.key] = struct{}{}
	w.s.mu.Unlock()

	return nil
}

func (s *trackingStore) Create(ctx context.Context, disk cluster.DiskID, name string) (io.WriteCloser, error) {
	w, err := s.MemStore.Create(ctx, disk, name)
	if err != nil {
		return nil, err
	}

	return trackingWriter{WriteCloser: w, s: s, key: string(disk) + "\x00" + name}, nil
}

func (s *trackingStore) Delete(ctx context.Context, disk cluster.DiskID, name string) error {
	if err := s.MemStore.Delete(ctx, disk, name); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.names, string(disk)+"\x00"+name)
	s.mu.Unlock()

	return nil
}

func (s *trackingStore) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}

	return out
}

// errPeer simulates an unreachable node: every operation fails.
type errPeer struct{}

var errNodeDown = errors.New("node down")

func (errPeer) Put(context.Context, cluster.DiskID, string, int64, io.Reader) error {
	return errNodeDown
}

func (errPeer) Get(context.Context, cluster.DiskID, string) (io.ReadCloser, int64, error) {
	return nil, 0, errNodeDown
}

func (errPeer) Stat(context.Context, cluster.DiskID, string) (int64, error) { return 0, errNodeDown }
func (errPeer) Delete(context.Context, cluster.DiskID, string) error        { return errNodeDown }

func (errPeer) List(context.Context, cluster.DiskID, string) ([]string, error) {
	return nil, errNodeDown
}

// fakeCluster is an in-process cluster: one tracking store per node, direct
// LocalPeer dialing, per-node kill switch.
type fakeCluster struct {
	topo   *cluster.Topology
	stores map[cluster.NodeID]*trackingStore

	mu   sync.Mutex
	down map[cluster.NodeID]bool
}

func newFakeCluster(nodes, disksPerNode int) *fakeCluster {
	fc := &fakeCluster{
		stores: make(map[cluster.NodeID]*trackingStore),
		down:   make(map[cluster.NodeID]bool),
	}

	topo := &cluster.Topology{Epoch: 1}

	for n := range nodes {
		id := cluster.NodeID("n" + strconv.Itoa(n))
		node := cluster.Node{ID: id, Rack: "r" + strconv.Itoa(n)}

		for d := range disksPerNode {
			node.Disks = append(node.Disks, cluster.Disk{ID: cluster.DiskID("d" + strconv.Itoa(d)), Weight: 1})
		}

		topo.Nodes = append(topo.Nodes, node)
		fc.stores[id] = newTrackingStore()
	}

	fc.topo = topo

	return fc
}

func (fc *fakeCluster) Peer(node cluster.Node) (Peer, error) {
	fc.mu.Lock()
	isDown := fc.down[node.ID]
	fc.mu.Unlock()

	if isDown {
		return errPeer{}, nil
	}

	return LocalPeer{Store: fc.stores[node.ID]}, nil
}

func (fc *fakeCluster) setDown(id cluster.NodeID, down bool) {
	fc.mu.Lock()
	fc.down[id] = down
	fc.mu.Unlock()
}

// allNames returns every stored name across the cluster.
func (fc *fakeCluster) allNames() []string {
	var out []string
	for _, s := range fc.stores {
		out = append(out, s.list()...)
	}

	return out
}

func (fc *fakeCluster) coordinator(t *testing.T, cfg Config) *Coordinator {
	t.Helper()

	cfg.Topology = StaticTopology{T: fc.topo}
	cfg.Peers = fc

	c, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func fixedScheme(s scheme.Scheme) SchemeFunc {
	return func(string) scheme.Scheme { return s }
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return b
}

func testSchemes() []scheme.Scheme {
	return []scheme.Scheme{
		{Kind: scheme.RF25},
		{Kind: scheme.RF3},
		{Kind: scheme.EC, K: 2, M: 1},
		{Kind: scheme.EC, K: 4, M: 2},
	}
}

// mustPut writes an object into the test bucket "b".
func mustPut(t *testing.T, c *Coordinator, key string, data []byte) *Sidecar {
	t.Helper()

	sc, err := c.Put(t.Context(), &PutRequest{
		Bucket: "b", Key: key, Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	require.NoError(t, err)

	return sc
}

func readObject(t *testing.T, c *Coordinator, key string) []byte {
	t.Helper()

	_, rc, err := c.Get(t.Context(), "b", key)
	require.NoError(t, err)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	return data
}

func TestPutGetRoundTrip(t *testing.T) {
	for _, s := range testSchemes() {
		for _, n := range []int{0, 1, 999, 4097, 1 << 20} {
			t.Run(fmt.Sprintf("%s/%d", s, n), func(t *testing.T) {
				fc := newFakeCluster(6, 2)
				c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})
				data := randBytes(n)

				sc := mustPut(t, c, "k/with spaces/файл", data)
				assert.Equal(t, int64(n), sc.Size)
				assert.Equal(t, s.String(), sc.Scheme)

				sum := md5.Sum(data) //nolint:gosec // Expected checksum.
				assert.Equal(t, hex.EncodeToString(sum[:]), sc.Checksum)
				assert.Equal(t, sc.Checksum, sc.ETag)

				c.Flush()

				got := readObject(t, c, "k/with spaces/файл")
				assert.True(t, bytes.Equal(data, got), "round-trip")

				// Every planned fragment must exist after the async remainder.
				plan, err := fragment.Plan(fc.topo, s, placement.ObjectKey("b", "k/with spaces/файл"), int64(n))
				require.NoError(t, err)

				for _, item := range plan {
					name := fragmentName("b", "k/with spaces/файл", sc.Generation, item.Index)
					size, err := fc.stores[item.Target.Node].Stat(t.Context(), item.Target.Disk, name)
					require.NoError(t, err, "fragment %d on %s", item.Index, item.Target.Node)
					assert.Equal(t, item.Size, size, "fragment %d size", item.Index)
				}
			})
		}
	}
}

func TestGetNotFound(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	_, _, err := c.Get(t.Context(), "b", "missing")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = c.Stat(t.Context(), "b", "missing")
	require.ErrorIs(t, err, ErrNotFound)

	err = c.Delete(t.Context(), "b", "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestInsufficientTargetsRefused(t *testing.T) {
	// Two disks cannot host any 3-target scheme; the write must be refused
	// with nothing stored.
	fc := newFakeCluster(2, 1)
	c := fc.coordinator(t, Config{})

	_, err := c.Put(t.Context(), &PutRequest{Bucket: "b", Key: "k", Size: 3, Body: bytes.NewReader([]byte("abc"))})
	require.ErrorIs(t, err, ErrInsufficientTargets)
	assert.Empty(t, fc.allNames(), "refused write must leave nothing behind")
}

func TestSyncFailureRefusesWrite(t *testing.T) {
	for _, s := range testSchemes() {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(6, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

			data := randBytes(2048)
			plan, err := fragment.Plan(fc.topo, s, placement.ObjectKey("b", "k"), int64(len(data)))
			require.NoError(t, err)

			// Kill the second quorum target: the sync phase cannot complete.
			fc.setDown(plan[1].Target.Node, true)

			_, err = c.Put(t.Context(), &PutRequest{Bucket: "b", Key: "k", Size: int64(len(data)), Body: bytes.NewReader(data)})
			require.Error(t, err)

			c.Flush()

			// While the node is down, reads report unreachability, never a
			// phantom "not found".
			_, _, err = c.Get(t.Context(), "b", "k")
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrNotFound, "unreachable must not read as gone")

			// Once it recovers there is no committed state anywhere.
			fc.setDown(plan[1].Target.Node, false)

			_, _, err = c.Get(t.Context(), "b", "k")
			require.ErrorIs(t, err, ErrNotFound, "refused write must not be readable")
			assert.Empty(t, fc.allNames(), "refused write must be rolled back")
		})
	}
}

func TestReplicaFailover(t *testing.T) {
	for _, s := range []scheme.Scheme{{Kind: scheme.RF25}, {Kind: scheme.RF3}} {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(4, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})
			data := randBytes(9000)

			sc := mustPut(t, c, "k", data)
			c.Flush()

			plan, err := fragment.Plan(fc.topo, s, placement.ObjectKey("b", "k"), int64(len(data)))
			require.NoError(t, err)

			// Losing the primary replica must not lose reads.
			frag0 := fragmentName("b", "k", sc.Generation, 0)
			require.NoError(t, fc.stores[plan[0].Target.Node].Delete(t.Context(), plan[0].Target.Disk, frag0))
			assert.True(t, bytes.Equal(data, readObject(t, c, "k")), "failover to second replica")

			// Losing every full replica is unrecoverable on the read path
			// (parity-based rebuild is the repair worker's job).
			for i := 1; i < s.FullReplicas(); i++ {
				name := fragmentName("b", "k", sc.Generation, i)
				require.NoError(t, fc.stores[plan[i].Target.Node].Delete(t.Context(), plan[i].Target.Disk, name))
			}

			_, _, err = c.Get(t.Context(), "b", "k")
			require.ErrorIs(t, err, ErrUnrecoverable)
		})
	}
}

func TestECDegradedRead(t *testing.T) {
	s := scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}
	fc := newFakeCluster(6, 1)
	c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})
	data := randBytes(50_000)

	mustPut(t, c, "k", data)
	c.Flush()

	plan, err := fragment.Plan(fc.topo, s, placement.ObjectKey("b", "k"), int64(len(data)))
	require.NoError(t, err)

	// Any m=2 nodes down: reads reconstruct.
	fc.setDown(plan[0].Target.Node, true)
	fc.setDown(plan[4].Target.Node, true)
	assert.True(t, bytes.Equal(data, readObject(t, c, "k")), "degraded read")

	// m+1 shards gone: the reader must fail with ErrUnrecoverable. The
	// sidecar must stay reachable (it lives on more targets than the lost
	// shards).
	fc.setDown(plan[1].Target.Node, true)

	_, rc, err := c.Get(t.Context(), "b", "k")
	require.NoError(t, err, "sidecar still reachable")

	_, err = io.ReadAll(rc)
	require.ErrorIs(t, err, ErrUnrecoverable)
	require.NoError(t, rc.Close())
}

func TestDelete(t *testing.T) {
	for _, s := range testSchemes() {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(6, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

			mustPut(t, c, "k", randBytes(4096))
			c.Flush()

			require.NoError(t, c.Delete(t.Context(), "b", "k"))

			_, _, err := c.Get(t.Context(), "b", "k")
			require.ErrorIs(t, err, ErrNotFound)
			require.ErrorIs(t, c.Delete(t.Context(), "b", "k"), ErrNotFound, "second delete")
			assert.Empty(t, fc.allNames(), "delete must leave no fragments")
		})
	}
}

func TestOverwriteCleansPreviousGeneration(t *testing.T) {
	for _, s := range testSchemes() {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(6, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

			first := mustPut(t, c, "k", randBytes(3000))
			c.Flush()

			second := randBytes(5000)
			sc := mustPut(t, c, "k", second)
			c.Flush()

			require.NotEqual(t, first.Generation, sc.Generation)
			assert.True(t, bytes.Equal(second, readObject(t, c, "k")), "overwrite served")

			// Exactly one generation may remain: copies fragments + sidecars.
			want := s.Copies() * 2
			assert.Len(t, fc.allNames(), want, "stale generation must be cleaned up")
		})
	}
}

func TestSchemeChangeOverwrite(t *testing.T) {
	// An object written under the bucket's old EC scheme is overwritten after
	// the bucket switched to RF=2.5: the old shards AND the old sidecars on
	// targets outside the new placement must be cleaned up, or a stale copy
	// would keep serving there.
	fc := newFakeCluster(6, 1)
	ec := scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}

	old := fc.coordinator(t, Config{Scheme: fixedScheme(ec)})
	mustPut(t, old, "k", randBytes(2000))
	old.Flush()

	c := fc.coordinator(t, Config{Scheme: fixedScheme(scheme.Scheme{Kind: scheme.RF25})})
	data := randBytes(4000)
	mustPut(t, c, "k", data)
	c.Flush()

	assert.True(t, bytes.Equal(data, readObject(t, c, "k")))
	assert.Len(t, fc.allNames(), 6, "only the RF=2.5 generation (3 fragments + 3 sidecars) may remain")

	// The old-scheme coordinator (stale config) must also see the new object,
	// via the sidecar candidate fallback.
	assert.True(t, bytes.Equal(data, readObject(t, old, "k")), "read across scheme change")
}

func TestAsyncErrorReported(t *testing.T) {
	fc := newFakeCluster(4, 1)

	var (
		mu     sync.Mutex
		hooked []error
	)

	c := fc.coordinator(t, Config{
		OnAsyncError: func(_, _ string, err error) {
			mu.Lock()
			defer mu.Unlock()

			hooked = append(hooked, err)
		},
	})

	data := randBytes(1000)
	plan, err := fragment.Plan(fc.topo, scheme.Default, placement.ObjectKey("b", "k"), int64(len(data)))
	require.NoError(t, err)

	// The third target is down: the quorum write succeeds, the async parity
	// task fails and must be reported.
	fc.setDown(plan[2].Target.Node, true)

	mustPut(t, c, "k", data)
	c.Flush()

	mu.Lock()
	defer mu.Unlock()

	require.NotEmpty(t, hooked, "async failure must reach the hook")

	// The object stays readable at quorum.
	fc.setDown(plan[2].Target.Node, false)
	assert.True(t, bytes.Equal(data, readObject(t, c, "k")))
}

func TestConcurrentPuts(t *testing.T) {
	fc := newFakeCluster(6, 2)
	c := fc.coordinator(t, Config{})

	var wg sync.WaitGroup

	for i := range 16 {
		wg.Go(func() {
			key := "k" + strconv.Itoa(i)
			data := randBytes(10_000 + i)

			sc, err := c.Put(context.Background(), &PutRequest{
				Bucket: "b", Key: key, Size: int64(len(data)), Body: bytes.NewReader(data),
			})
			assert.NoError(t, err)

			if sc == nil {
				return
			}

			_, rc, err := c.Get(context.Background(), "b", key)
			if !assert.NoError(t, err) {
				return
			}

			got, err := io.ReadAll(rc)
			assert.NoError(t, err)
			assert.NoError(t, rc.Close())
			assert.True(t, bytes.Equal(data, got), "key %s", key)
		})
	}

	wg.Wait()
	c.Flush()
}

// TestHTTPCluster runs three real nodes — tracking stores behind
// authenticated transport servers — and drives the coordinator over actual
// HTTP, including reading through a different node than the writer (any node
// serves any key).
func TestHTTPCluster(t *testing.T) {
	secret := transport.Secret(randBytes(32))
	topo := &cluster.Topology{Epoch: 1}
	stores := make(map[cluster.NodeID]*trackingStore)

	for i := range 3 {
		id := cluster.NodeID("n" + strconv.Itoa(i))
		store := newTrackingStore()
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

	node := func(t *testing.T, self cluster.NodeID) *Coordinator {
		t.Helper()

		c, err := New(Config{
			Topology: StaticTopology{T: topo},
			Peers:    NewHTTPPeers(self, stores[self], secret, nil),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })

		return c
	}

	writer := node(t, "n0")
	reader := node(t, "n2")

	data := randBytes(300_000)
	sc := mustPut(t, writer, "видео/clip 01.mp4", data)
	writer.Flush()

	assert.Equal(t, "rf2.5", sc.Scheme)
	assert.True(t, bytes.Equal(data, readObject(t, reader, "видео/clip 01.mp4")), "cross-node read")

	// Scatter-gather listing and the bucket registry over real transport.
	require.NoError(t, writer.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	listed, err := reader.ListObjects(t.Context(), "b", "видео/")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "видео/clip 01.mp4", listed[0].Key)

	buckets, err := reader.ListBuckets(t.Context())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, "b", buckets[0].Name)

	require.NoError(t, reader.DeleteBucket(t.Context(), "b"))
	require.NoError(t, reader.Delete(t.Context(), "b", "видео/clip 01.mp4"))

	_, _, err = writer.Get(t.Context(), "b", "видео/clip 01.mp4")
	require.ErrorIs(t, err, ErrNotFound)

	for id, s := range stores {
		assert.Empty(t, s.list(), "store %s must be empty after delete", id)
	}
}
