package clusterstore

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// rebalanceRunner builds a repairer the way `fs cluster rebalance` does: a
// pure client under a synthetic node ID that matches no topology node.
func rebalanceRunner(t *testing.T, c *Coordinator, verify bool) *Repairer {
	t.Helper()

	return newRepairer(t, c, "rebalance/test/1", verify)
}

func TestRebalance(t *testing.T) {
	for _, s := range []scheme.Scheme{{Kind: scheme.RF25}, {Kind: scheme.RF3}, {Kind: scheme.EC, K: 2, M: 1}} {
		t.Run(s.String(), func(t *testing.T) {
			fc := newFakeCluster(4, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(s)})

			require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

			payload := putMany(t, c, 12)

			gens := make(map[string]string)

			for key := range payload {
				sc, err := c.Stat(t.Context(), "b", key)
				require.NoError(t, err)

				gens[key] = sc.Generation
			}

			oldTopo := fc.topo
			fc.addNodes()
			require.NotEmpty(t, movedKeys(t, s, oldTopo, fc.topo, payload))

			r := rebalanceRunner(t, c, true)

			// The dry-run plan sees the pending moves, attributed to the
			// destination nodes.
			plan, err := r.PlanRebalance(t.Context())
			require.NoError(t, err)
			assert.Equal(t, len(payload), plan.Objects)
			assert.Positive(t, plan.MisplacedObjects)
			assert.Positive(t, plan.MisplacedBytes)
			assert.Zero(t, plan.Unplannable)
			assert.NotEmpty(t, plan.Nodes)

			// One rebalance pass converges everything to the new placement.
			var cursors []RebalanceCursor

			report, err := r.Rebalance(t.Context(), RebalanceOptions{
				Concurrency: 3,
				Checkpoint: func(_ context.Context, cur RebalanceCursor) error {
					cursors = append(cursors, cur)
					return nil
				},
			})
			require.NoError(t, err)
			assert.Equal(t, 1, report.Buckets)
			assert.Equal(t, len(payload), report.Objects)
			assert.Positive(t, report.Relocated)
			assert.Zero(t, report.Failed)
			require.NotEmpty(t, cursors)
			assert.Equal(t, "b", cursors[len(cursors)-1].Bucket)

			// Checkpoints advance in key order.
			for i := 1; i < len(cursors); i++ {
				assert.Less(t, cursors[i-1].Key, cursors[i].Key)
			}

			verifyPlacement(t, fc, s, payload, gens)

			for key, want := range payload {
				assert.True(t, bytes.Equal(want, readObject(t, c, key)), "post-rebalance read %s", key)
			}

			// Converged: the plan is empty and a second pass changes nothing.
			plan, err = r.PlanRebalance(t.Context())
			require.NoError(t, err)
			assert.Zero(t, plan.MisplacedObjects)
			assert.Zero(t, plan.MisplacedBytes)
			assert.Empty(t, plan.Nodes)

			report, err = r.Rebalance(t.Context(), RebalanceOptions{})
			require.NoError(t, err)
			assert.Zero(t, report.Relocated)
		})
	}
}

func TestRebalanceResume(t *testing.T) {
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	payload := putMany(t, c, 10)

	fc.addNodes()

	scs, err := c.ListObjects(t.Context(), "b", "")
	require.NoError(t, err)
	require.Len(t, scs, len(payload))

	// Resume after the 4th key: only the tail is processed.
	resume := RebalanceCursor{Bucket: "b", Key: scs[3].Key}

	var seen []string

	r := rebalanceRunner(t, c, false)

	report, err := r.Rebalance(t.Context(), RebalanceOptions{
		Resume: resume,
		OnObject: func(_, key string, _ *RepairReport, err error) {
			require.NoError(t, err)

			seen = append(seen, key)
		},
	})
	require.NoError(t, err)
	assert.Equal(t, len(payload)-4, report.Objects)
	assert.Len(t, seen, len(payload)-4)

	for _, key := range seen {
		assert.Greater(t, key, resume.Key)
	}

	// A cursor past the whole bucket processes nothing.
	report, err = r.Rebalance(t.Context(), RebalanceOptions{
		Resume: RebalanceCursor{Bucket: "b", Key: "\xff"},
	})
	require.NoError(t, err)
	assert.Zero(t, report.Objects)

	// A cursor in a bucket past "b" skips the bucket entirely.
	report, err = r.Rebalance(t.Context(), RebalanceOptions{
		Resume: RebalanceCursor{Bucket: "c", Key: ""},
	})
	require.NoError(t, err)
	assert.Zero(t, report.Buckets)
}

func TestRebalanceCheckpointErrorAborts(t *testing.T) {
	fc := newFakeCluster(4, 1)
	c := fc.coordinator(t, Config{})

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))
	putMany(t, c, 8)

	boom := assert.AnError
	r := rebalanceRunner(t, c, false)

	report, err := r.Rebalance(t.Context(), RebalanceOptions{
		Concurrency: 2,
		Checkpoint: func(context.Context, RebalanceCursor) error {
			return boom
		},
	})
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 2, report.Objects, "aborts after the first batch")
}

func TestRebalanceThrottled(t *testing.T) {
	// Rebalance through a bandwidth-limited dialer: same convergence, and the
	// limiter actually saw the moved bytes.
	s := scheme.Scheme{Kind: scheme.RF3}
	fc := newFakeCluster(4, 1)

	// A tiny burst forces every fragment stream through the limiter in small
	// chunks; the huge rate keeps the test fast.
	limiter := rate.NewLimiter(1<<30, 512)

	c, err := New(Config{
		Topology: fakeTopoSource{fc: fc},
		Peers:    &ThrottledPeers{Dialer: fc, Limiter: limiter},
		Scheme:   fixedScheme(s),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	payload := putMany(t, c, 8)

	fc.addNodes()

	r := rebalanceRunner(t, c, true)

	report, err := r.Rebalance(t.Context(), RebalanceOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.Failed)

	for key, want := range payload {
		assert.True(t, bytes.Equal(want, readObject(t, c, key)), "throttled rebalance read %s", key)
	}
}

func TestRebalanceCursorRoundTrip(t *testing.T) {
	cur := RebalanceCursor{Bucket: "photos", Key: "a/b c/d"}

	raw, err := cur.Encode()
	require.NoError(t, err)

	got, err := DecodeRebalanceCursor(raw)
	require.NoError(t, err)
	assert.Equal(t, cur, got)

	_, err = DecodeRebalanceCursor("not json")
	require.Error(t, err)
}
