package clusterstore

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
	"github.com/go-faster/fs/storagetest"
)

// transportClusterStorage builds a real N-node cluster — tracking stores
// behind authenticated HTTP transport servers — and returns the fs.Storage
// served through node n0 (any node serves any key). Unlike TestConformance's
// in-process fakeCluster (direct LocalPeer dialing), every fragment op here
// crosses the actual wire.
func transportClusterStorage(tb testing.TB, s scheme.Scheme, nodeCount, disksPerNode int) fs.Storage {
	tb.Helper()

	secret := transport.Secret(randBytes(32))
	topo := &cluster.Topology{Epoch: 1}
	stores := make(map[cluster.NodeID]*trackingStore, nodeCount)

	for i := range nodeCount {
		id := cluster.NodeID("n" + strconv.Itoa(i))
		store := newTrackingStore()
		srv := httptest.NewServer(transport.NewServer(store, secret))
		tb.Cleanup(srv.Close)

		stores[id] = store

		node := cluster.Node{ID: id, Addr: srv.Listener.Addr().String(), Rack: "r" + strconv.Itoa(i)}
		for d := range disksPerNode {
			node.Disks = append(node.Disks, cluster.Disk{ID: cluster.DiskID("d" + strconv.Itoa(d)), Weight: 1})
		}

		topo.Nodes = append(topo.Nodes, node)
	}

	// A dedicated client per ephemeral cluster with keep-alives off: storagetest
	// builds a fresh cluster (new httptest servers on fresh ports) per case and
	// tears it down, so a shared/pooled connection could be reused against a
	// closed-then-reopened port and fail mid-request. No pooling, no such race.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	tb.Cleanup(client.CloseIdleConnections)

	c, err := New(Config{
		Topology: StaticTopology{T: topo},
		Peers:    NewHTTPPeers("n0", stores["n0"], secret, client),
		Scheme:   fixedScheme(s),
	})
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = c.Close() })

	return NewStorage(c)
}

// TestConformanceOverTransport runs the full fs.Storage conformance suite
// against a real 3-node cluster over the authenticated HTTP transport, for
// every scheme a 3-domain cluster can host. This is the ROADMAP Phase 7
// "storagetest.Run green against 3 in-process nodes" gate, exercised over the
// wire rather than direct in-process peers.
func TestConformanceOverTransport(t *testing.T) {
	for _, s := range []scheme.Scheme{
		{Kind: scheme.RF25},
		{Kind: scheme.RF3},
		{Kind: scheme.EC, K: 2, M: 1},
	} {
		t.Run(s.String(), func(t *testing.T) {
			storagetest.Run(t, func(tb testing.TB) fs.Storage {
				return transportClusterStorage(tb, s, 3, 2)
			})
		})
	}
}

// TestOneNodeDownReadsAndWritesContinue is the Phase 7 availability gate: with
// one node lost, the cluster keeps serving reads (failover) and accepting
// writes (placement routes around the departed node once it leaves the
// topology, as its etcd lease would expire in production).
func TestOneNodeDownReadsAndWritesContinue(t *testing.T) {
	for _, s := range []scheme.Scheme{{Kind: scheme.RF25}, {Kind: scheme.RF3}, {Kind: scheme.EC, K: 2, M: 1}} {
		t.Run(s.String(), func(t *testing.T) {
			// Four nodes: losing one still leaves the three distinct domains
			// every 3-fragment scheme needs.
			fc := newFakeCluster(4, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

			require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

			before := putMany(t, c, 8)

			// One node fails and leaves the topology (lease expiry): a new epoch
			// without it, so placement for new writes routes around it.
			gone := fc.topo.Nodes[3].ID
			fc.setDown(gone, true)

			next := &cluster.Topology{Epoch: fc.topo.Epoch + 1}
			for _, n := range fc.topo.Nodes {
				if n.ID != gone {
					next.Nodes = append(next.Nodes, n)
				}
			}

			fc.setTopology(next)

			// Reads of existing objects continue by failing over to the
			// surviving replicas/shards (at most one fragment was on the lost
			// node; every scheme here tolerates one domain).
			for key, want := range before {
				assert.True(t, bytes.Equal(want, readObject(t, c, key)), "read %s with a node down", key)
			}

			// New writes succeed on the surviving three-node cluster and read
			// back — through a node other than the one that placed them is
			// covered by the transport test; here any node serves.
			fresh := randBytes(3000)

			_, err := c.Put(t.Context(), &PutRequest{Bucket: "b", Key: "fresh", Size: int64(len(fresh)), Body: bytes.NewReader(fresh)})
			require.NoError(t, err, "write must succeed with one node down")

			c.Flush()
			assert.True(t, bytes.Equal(fresh, readObject(t, c, "fresh")), "read back a write made while a node was down")
		})
	}
}

// TestBothReplicaHoldersLostIsDocumentedLoss pins the documented loss case:
// for RF2.5, losing BOTH full replicas is unrecoverable even though the
// half-parity survives (parity alone cannot reconstruct the object) — a
// correct, explicit error, not a silent wrong answer.
func TestBothReplicaHoldersLostIsDocumentedLoss(t *testing.T) {
	s := scheme.Scheme{Kind: scheme.RF25}
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

	data := randBytes(6000)
	sc := mustPut(t, c, "k", data)
	c.Flush()

	plan := testPlan(t, fc, s, len(data))

	// Drop both full replicas (indices 0 and 1); the parity (index 2) remains.
	for i := range s.FullReplicas() {
		name := fragmentName("b", "k", sc.Generation, i)
		require.NoError(t, fc.stores[plan[i].Target.Node].Delete(t.Context(), plan[i].Target.Disk, name))
	}

	_, _, err := c.Get(t.Context(), "b", "k")
	require.ErrorIs(t, err, ErrUnrecoverable, "both full replicas gone → unrecoverable (documented RF2.5 loss)")
}
