package clusterstore

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

func newRepairer(t *testing.T, c *Coordinator, self cluster.NodeID, verify bool) *Repairer {
	t.Helper()

	r, err := NewRepairer(RepairerConfig{Coordinator: c, Self: self, Verify: verify})
	require.NoError(t, err)

	return r
}

// testPlan recomputes the test object ("b"/"k") plan.
func testPlan(t *testing.T, fc *fakeCluster, s scheme.Scheme, size int) []fragment.Item {
	t.Helper()

	plan, err := fragment.Plan(fc.topo, s, placement.ObjectKey("b", "k"), int64(size))
	require.NoError(t, err)

	return plan
}

func TestRepairRebuildsEveryFragment(t *testing.T) {
	for _, s := range testSchemes() {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(6, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})
			data := randBytes(20_000)

			sc := mustPut(t, c, "k", data)
			c.Flush()

			plan := testPlan(t, fc, s, len(data))
			r := newRepairer(t, c, plan[0].Target.Node, false)

			// Destroy each fragment in turn; repair must restore it exactly.
			for i := range plan {
				name := fragmentName("b", "k", sc.Generation, plan[i].Index)
				require.NoError(t, fc.stores[plan[i].Target.Node].Delete(t.Context(), plan[i].Target.Disk, name))

				rep, err := r.RepairObject(t.Context(), "b", "k")
				require.NoError(t, err, "fragment %d", i)
				assert.Equal(t, 1, rep.RebuiltFragments, "fragment %d", i)

				size, err := fc.stores[plan[i].Target.Node].Stat(t.Context(), plan[i].Target.Disk, name)
				require.NoError(t, err, "fragment %d restored", i)
				assert.Equal(t, plan[i].Size, size)

				assert.True(t, bytes.Equal(data, readObject(t, c, "k")), "object intact after repair %d", i)
			}

			// A healthy object is a no-op pass.
			rep, err := r.RepairObject(t.Context(), "b", "k")
			require.NoError(t, err)
			assert.False(t, rep.Changed(), "healthy object must not be touched")
		})
	}
}

func TestRepairMultipleECShards(t *testing.T) {
	s := scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}
	fc := newFakeCluster(6, 1)
	c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})
	data := randBytes(50_000)

	sc := mustPut(t, c, "k", data)
	c.Flush()

	plan := testPlan(t, fc, s, len(data))
	r := newRepairer(t, c, plan[0].Target.Node, false)

	// Lose m=2 shards at once.
	for _, i := range []int{1, 4} {
		name := fragmentName("b", "k", sc.Generation, plan[i].Index)
		require.NoError(t, fc.stores[plan[i].Target.Node].Delete(t.Context(), plan[i].Target.Disk, name))
	}

	rep, err := r.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, 2, rep.RebuiltFragments)
	assert.True(t, bytes.Equal(data, readObject(t, c, "k")))

	// Losing more than m is unrecoverable.
	for _, i := range []int{0, 2, 3} {
		name := fragmentName("b", "k", sc.Generation, plan[i].Index)
		require.NoError(t, fc.stores[plan[i].Target.Node].Delete(t.Context(), plan[i].Target.Disk, name))
	}

	_, err = r.RepairObject(t.Context(), "b", "k")
	require.ErrorIs(t, err, ErrUnrecoverable)
}

func TestRepairRewritesSidecars(t *testing.T) {
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	mustPut(t, c, "k", randBytes(1000))
	c.Flush()

	plan := testPlan(t, fc, scheme.Default, 1000)
	r := newRepairer(t, c, plan[0].Target.Node, false)

	// Drop the sidecar on two of three targets.
	for _, i := range []int{1, 2} {
		require.NoError(t, fc.stores[plan[i].Target.Node].Delete(t.Context(), plan[i].Target.Disk, sidecarName("b", "k")))
	}

	rep, err := r.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, 2, rep.RewrittenSidecars)

	for i := range plan {
		_, err := fc.stores[plan[i].Target.Node].Stat(t.Context(), plan[i].Target.Disk, sidecarName("b", "k"))
		require.NoError(t, err, "sidecar on target %d", i)
	}
}

func TestRepairSweepsStaleGeneration(t *testing.T) {
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	// Simulate a failed old-generation cleanup: plant fragments of a bogus
	// generation next to the committed one.
	sc := mustPut(t, c, "k", randBytes(500))
	c.Flush()

	plan := testPlan(t, fc, scheme.Default, 500)

	stale := fragmentName("b", "k", "deadbeef00000000", 0)
	w, err := fc.stores[plan[0].Target.Node].Create(t.Context(), plan[0].Target.Disk, stale)
	require.NoError(t, err)

	_, _ = w.Write([]byte("stale"))
	require.NoError(t, w.Close())

	r := newRepairer(t, c, plan[0].Target.Node, false)
	r.sweepGrace = time.Nanosecond

	// First pass: the generation is unattributed (no record names it) — it
	// might be another node's write mid-commit, so it is only sighted.
	rep, err := r.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Zero(t, rep.DeletedStale, "an unattributed generation gets a grace period")

	_, err = fc.stores[plan[0].Target.Node].Stat(t.Context(), plan[0].Target.Disk, stale)
	require.NoError(t, err)

	// Second pass after the grace: still no record — reclaimed as garbage.
	rep, err = r.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.DeletedStale)

	_, err = fc.stores[plan[0].Target.Node].Stat(t.Context(), plan[0].Target.Disk, stale)
	require.Error(t, err, "expired stale generation must be swept")

	// The committed generation is untouched.
	name := fragmentName("b", "k", sc.Generation, 0)
	_, err = fc.stores[plan[0].Target.Node].Stat(t.Context(), plan[0].Target.Disk, name)
	require.NoError(t, err)
}

func TestRepairPassAbortsOnConcurrentOverwrite(t *testing.T) {
	// Chaos-suite regression: a repair pass whose authoritative view predates
	// a concurrent overwrite (committed by another node mid-pass) must abort
	// with the newer record — never downgrade its sidecars or sweep its
	// fragments (that destroyed an acked EC write).
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	sc1 := mustPut(t, c, "k", randBytes(2000))
	c.Flush()

	staleView := *sc1 // The repairer's outdated authoritative record.

	data2 := randBytes(2500)
	sc2 := mustPut(t, c, "k", data2)
	c.Flush()

	r := newRepairer(t, c, "n0", true)
	r.sweepGrace = time.Nanosecond

	report := &RepairReport{}
	known := map[string]struct{}{staleView.Generation: {}}

	err := r.repairPass(t.Context(), &staleView, known, nil, report)

	var newer *newerRecordError
	require.ErrorAs(t, err, &newer, "the pass must detect the concurrent overwrite")
	assert.Equal(t, sc2.Generation, newer.sc.Generation)

	// The newer write is fully intact: same content, sidecars not downgraded.
	assert.True(t, bytes.Equal(data2, readObject(t, c, "k")))

	got, err := c.Stat(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, sc2.Generation, got.Generation)

	// A full repair converges on the newer record without changes.
	rep, err := r.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.False(t, rep.Changed())
}

func TestRepairDetectsBitRot(t *testing.T) {
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})
	data := randBytes(4000)

	sc := mustPut(t, c, "k", data)
	c.Flush()

	plan := testPlan(t, fc, scheme.Default, len(data))

	// Flip the primary replica's payload (same size, wrong bytes).
	corrupt := randBytes(len(data))
	name := fragmentName("b", "k", sc.Generation, 0)
	w, err := fc.stores[plan[0].Target.Node].Create(t.Context(), plan[0].Target.Disk, name)
	require.NoError(t, err)

	_, _ = w.Write(corrupt)
	require.NoError(t, w.Close())

	// Without verify the torn payload passes (size matches).
	r := newRepairer(t, c, plan[0].Target.Node, false)
	rep, err := r.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, 0, rep.CorruptReplicas)

	// With verify it is detected and rebuilt from the healthy replica.
	rv := newRepairer(t, c, plan[0].Target.Node, true)
	rep, err = rv.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.CorruptReplicas)
	assert.Equal(t, 1, rep.RebuiltFragments)

	rc, _, err := fc.stores[plan[0].Target.Node].Open(t.Context(), plan[0].Target.Disk, name)
	require.NoError(t, err)

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.True(t, bytes.Equal(data, got), "replica restored to true content")
}

func TestRepairCompletesMissedRemainder(t *testing.T) {
	// A write whose async remainder failed (third target down) is completed
	// by a later repair pass once the node recovers — the backfill path.
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})
	data := randBytes(3000)

	plan := testPlan(t, fc, scheme.Default, len(data))
	fc.setDown(plan[2].Target.Node, true)

	sc := mustPut(t, c, "k", data)
	c.Flush()

	// Node recovers; parity and its sidecar are still missing.
	fc.setDown(plan[2].Target.Node, false)

	r := newRepairer(t, c, plan[0].Target.Node, false)

	rep, err := r.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.RebuiltFragments, "parity backfilled")
	assert.Equal(t, 1, rep.RewrittenSidecars, "sidecar extended")

	name := fragmentName("b", "k", sc.Generation, 2)
	size, err := fc.stores[plan[2].Target.Node].Stat(t.Context(), plan[2].Target.Disk, name)
	require.NoError(t, err)
	assert.Equal(t, plan[2].Size, size)
}

func TestRepairConvergesDivergentSidecars(t *testing.T) {
	// Two sidecar replicas disagree (a torn overwrite): repair must converge
	// every target to the newest record and rebuild its generation.
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	first := mustPut(t, c, "k", randBytes(1000))
	c.Flush()

	second := mustPut(t, c, "k", randBytes(2000))
	c.Flush()

	require.NotEqual(t, first.Generation, second.Generation)

	plan := testPlan(t, fc, scheme.Default, 2000)

	// Regress target 1's sidecar to the first write's record.
	oldData, err := first.encode()
	require.NoError(t, err)
	require.NoError(t, putBytes(t.Context(), LocalPeer{Store: fc.stores[plan[1].Target.Node]}, plan[1].Target.Disk, sidecarName("b", "k"), oldData))

	r := newRepairer(t, c, plan[0].Target.Node, false)

	rep, err := r.RepairObject(t.Context(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.RewrittenSidecars, "stale sidecar replica rewritten")

	// Every target now serves the newest generation.
	for i := range plan {
		sc, err := readSidecarFrom(t.Context(), LocalPeer{Store: fc.stores[plan[i].Target.Node]}, plan[i].Target.Disk, sidecarName("b", "k"))
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, second.Generation, sc.Generation, "target %d", i)
	}
}

func TestRepairNotFound(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})
	r := newRepairer(t, c, "n0", false)

	_, err := r.RepairObject(t.Context(), "b", "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestScrubHealsCluster(t *testing.T) {
	for _, s := range []scheme.Scheme{{Kind: scheme.RF25}, {Kind: scheme.EC, K: 2, M: 1}} {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(4, 2)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

			payload := map[string][]byte{}
			for _, key := range []string{"a", "b/c", "d"} {
				payload[key] = randBytes(5000)
				mustPut(t, c, key, payload[key])
			}

			c.Flush()

			// Wipe one entire node's stores (disk loss). Its fragments are
			// gone; every object keeps quorum elsewhere.
			victim := fc.topo.Nodes[1].ID
			for _, disk := range []cluster.DiskID{"d0", "d1"} {
				names, err := fc.stores[victim].List(context.Background(), disk, "")
				require.NoError(t, err)

				for _, n := range names {
					require.NoError(t, fc.stores[victim].Delete(context.Background(), disk, n))
				}
			}

			// Scrub from every node: each repairs the objects its disks know.
			var healed ScrubReport

			for _, n := range fc.topo.Nodes {
				r := newRepairer(t, c, n.ID, true)

				rep, err := r.Scrub(t.Context())
				require.NoError(t, err)
				assert.Zero(t, rep.Failed, "node %s scrub failures", n.ID)

				healed.Repaired += rep.Repaired
				healed.Totals.add(&rep.Totals)
			}

			assert.Positive(t, healed.Totals.RebuiltFragments+healed.Totals.RewrittenSidecars,
				"the wiped node's state must be rebuilt")

			// Every object reads back and a final scrub pass is clean.
			for key, want := range payload {
				assert.True(t, bytes.Equal(want, readObject(t, c, key)), "key %s", key)
			}

			r := newRepairer(t, c, fc.topo.Nodes[0].ID, true)

			rep, err := r.Scrub(t.Context())
			require.NoError(t, err)
			assert.Zero(t, rep.Repaired, "second pass must be a no-op")
			assert.Zero(t, rep.Failed)
		})
	}
}

func TestScrubSkipsUnknownDirs(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	// A refused write's garbage: fragments without any sidecar.
	w, err := fc.stores["n0"].Create(t.Context(), "d0", "obj/aaaa/bbbb/cafe.f0")
	require.NoError(t, err)

	_, _ = w.Write([]byte("garbage"))
	require.NoError(t, w.Close())

	r := newRepairer(t, c, "n0", false)

	rep, err := r.Scrub(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, rep.UnknownDirs)
	assert.Zero(t, rep.Objects)

	// Untouched: sweeping undecidable dirs needs mtimes (follow-up).
	_, err = fc.stores["n0"].Stat(t.Context(), "d0", "obj/aaaa/bbbb/cafe.f0")
	require.NoError(t, err)
}
