package clusterstore

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// putMany writes n keys and returns their payloads.
func putMany(t *testing.T, c *Coordinator, n int) map[string][]byte {
	t.Helper()

	payload := make(map[string][]byte, n)

	for i := range n {
		key := "k" + strconv.Itoa(i)
		payload[key] = randBytes(2000 + i)

		sc, err := c.Put(t.Context(), &PutRequest{
			Bucket: "b", Key: key, Size: int64(len(payload[key])), Body: bytes.NewReader(payload[key]),
		})
		require.NoError(t, err)
		require.NotNil(t, sc)
	}

	c.Flush()

	return payload
}

// movedKeys returns the keys whose placement differs between two topologies.
func movedKeys(t *testing.T, s scheme.Scheme, a, b *cluster.Topology, payload map[string][]byte) []string {
	t.Helper()

	var moved []string

	for key, data := range payload {
		pa, err := fragment.Plan(a, s, placement.ObjectKey("b", key), int64(len(data)))
		require.NoError(t, err)

		pb, err := fragment.Plan(b, s, placement.ObjectKey("b", key), int64(len(data)))
		require.NoError(t, err)

		for i := range pa {
			if targetRef(pa[i].Target) != targetRef(pb[i].Target) {
				moved = append(moved, key)
				break
			}
		}
	}

	return moved
}

// verifyPlacement asserts every key's state lives exactly at its current-plan
// targets: all fragments and sidecars present there, nothing anywhere else.
func verifyPlacement(t *testing.T, fc *fakeCluster, s scheme.Scheme, payload map[string][]byte, gens map[string]string) {
	t.Helper()

	expected := make(map[string]map[string]struct{}) // node → set of disk\x00name

	for key, data := range payload {
		plan, err := fragment.Plan(fc.topo, s, placement.ObjectKey("b", key), int64(len(data)))
		require.NoError(t, err)

		for _, item := range plan {
			node := string(item.Target.Node)
			if expected[node] == nil {
				expected[node] = make(map[string]struct{})
			}

			expected[node][string(item.Target.Disk)+"\x00"+fragmentName("b", key, gens[key], item.Index)] = struct{}{}
			expected[node][string(item.Target.Disk)+"\x00"+sidecarName("b", key)] = struct{}{}
		}
	}

	for id, store := range fc.stores {
		got := store.list()

		want := expected[string(id)]
		assert.Len(t, got, len(want), "node %s name count", id)

		for _, n := range got {
			_, ok := want[n]
			assert.True(t, ok, "node %s holds unexpected name %q", id, n)
		}
	}
}

func TestRelocationAfterTopologyChange(t *testing.T) {
	for _, s := range []scheme.Scheme{{Kind: scheme.RF25}, {Kind: scheme.RF3}, {Kind: scheme.EC, K: 2, M: 1}} {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(4, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

			payload := putMany(t, c, 12)

			gens := make(map[string]string)

			for key := range payload {
				sc, err := c.Stat(t.Context(), "b", key)
				require.NoError(t, err)

				gens[key] = sc.Generation
			}

			oldTopo := fc.topo

			// Grow the cluster: placement moves for some keys.
			fc.addNodes()
			require.NotEmpty(t, movedKeys(t, s, oldTopo, fc.topo, payload), "growth must move some keys")

			// Before any relocation, every object stays readable via the
			// epoch fallback.
			for key, want := range payload {
				assert.True(t, bytes.Equal(want, readObject(t, c, key)), "pre-relocation read %s", key)
			}

			// Scrub every node (old and new): objects converge to the new
			// placement — copy, verify, then retire the old copies.
			for _, n := range fc.topo.Nodes {
				r := newRepairer(t, c, n.ID, true)

				rep, err := r.Scrub(t.Context())
				require.NoError(t, err)
				assert.Zero(t, rep.Failed, "node %s", n.ID)
			}

			// Everything now lives exactly at the new placement.
			verifyPlacement(t, fc, s, payload, gens)

			for key, want := range payload {
				assert.True(t, bytes.Equal(want, readObject(t, c, key)), "post-relocation read %s", key)
			}

			// A second full scrub is a no-op.
			for _, n := range fc.topo.Nodes {
				r := newRepairer(t, c, n.ID, true)

				rep, err := r.Scrub(t.Context())
				require.NoError(t, err)
				assert.Zero(t, rep.Repaired, "node %s second pass", n.ID)
				assert.Zero(t, rep.Failed)
			}
		})
	}
}

func TestRelocationSurvivesRestart(t *testing.T) {
	// The epoch memory is process-local. A coordinator started AFTER the
	// topology change has never seen the old epoch — scrub must still
	// relocate via the locally-discovered fragments, and reads recover once
	// relocation lands.
	s := scheme.Scheme{Kind: scheme.RF25}
	fc := newFakeCluster(4, 1)

	old := fc.coordinator(t, Config{Scheme: fixedScheme(s)})
	payload := putMany(t, old, 12)
	require.NoError(t, old.Close())

	oldTopo := fc.topo
	fc.addNodes()

	moved := movedKeys(t, s, oldTopo, fc.topo, payload)
	require.NotEmpty(t, moved)

	// Fresh coordinator: only the new epoch is known.
	fresh := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

	// Scrub every node with restart-fresh repairers.
	for _, n := range fc.topo.Nodes {
		r := newRepairer(t, fresh, n.ID, true)

		rep, err := r.Scrub(t.Context())
		require.NoError(t, err)
		assert.Zero(t, rep.Failed, "node %s", n.ID)
	}

	for key, want := range payload {
		assert.True(t, bytes.Equal(want, readObject(t, fresh, key)), "post-restart relocation read %s", key)
	}
}

func TestDeleteSpansEpochs(t *testing.T) {
	// Deleting a not-yet-relocated object must remove its state at the old
	// placement too.
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	payload := putMany(t, c, 8)

	fc.addNodes()

	for key := range payload {
		require.NoError(t, c.Delete(t.Context(), "b", key))
	}

	assert.Empty(t, fc.allNames(), "epoch-spanning delete must leave nothing")
}

func TestOverwriteAfterTopologyChangeCleansOldEpoch(t *testing.T) {
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	payload := putMany(t, c, 8)
	oldTopo := fc.topo

	fc.addNodes()

	moved := movedKeys(t, scheme.Default, oldTopo, fc.topo, payload)
	require.NotEmpty(t, moved)

	// Overwrite every key: the new generation goes to the new placement and
	// the old generation — including its old-epoch leftovers — is cleaned.
	gens := make(map[string]string)

	for key := range payload {
		data := randBytes(3000)
		payload[key] = data

		sc, err := c.Put(t.Context(), &PutRequest{
			Bucket: "b", Key: key, Size: int64(len(data)), Body: bytes.NewReader(data),
		})
		require.NoError(t, err)

		gens[key] = sc.Generation
	}

	c.Flush()

	verifyPlacement(t, fc, scheme.Default, payload, gens)

	for key, want := range payload {
		assert.True(t, bytes.Equal(want, readObject(t, c, key)), "key %s", key)
	}
}
