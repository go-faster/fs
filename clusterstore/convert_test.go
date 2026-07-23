package clusterstore

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// scrubAll runs a verifying scrub from every node and asserts no failures.
func scrubAll(t *testing.T, fc *fakeCluster, c *Coordinator) *ScrubReport {
	t.Helper()

	total := &ScrubReport{}

	for _, n := range fc.topo.Nodes {
		r := newRepairer(t, c, n.ID, true)

		rep, err := r.Scrub(t.Context())
		require.NoError(t, err)
		assert.Zero(t, rep.Failed, "node %s", n.ID)

		total.Objects += rep.Objects
		total.Repaired += rep.Repaired
		total.Totals.add(&rep.Totals)
	}

	return total
}

func TestSchemeConversion(t *testing.T) {
	for _, tc := range []struct{ from, to scheme.Scheme }{
		{scheme.Scheme{Kind: scheme.RF25}, scheme.Scheme{Kind: scheme.RF3}},
		{scheme.Scheme{Kind: scheme.RF3}, scheme.Scheme{Kind: scheme.EC, K: 2, M: 1}},
		{scheme.Scheme{Kind: scheme.EC, K: 2, M: 1}, scheme.Scheme{Kind: scheme.RF25}},
		{scheme.Scheme{Kind: scheme.RF25}, scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}},
	} {
		t.Run(tc.from.String()+"->"+tc.to.String(), func(t *testing.T) {
			fc := newFakeCluster(6, 1)
			c := fc.coordinator(t, Config{Scheme: fixedScheme(tc.from)})

			require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

			payload := putMany(t, c, 10)

			before := make(map[string]*Sidecar)

			for key := range payload {
				sc, err := c.Stat(t.Context(), "b", key)
				require.NoError(t, err)
				require.Equal(t, tc.from.String(), sc.Scheme)

				before[key] = sc
			}

			require.NoError(t, c.SetBucketScheme(t.Context(), "b", tc.to.String()))

			// One scrub converts every object; the identity survives, only the
			// scheme, generation and sequence change.
			rep := scrubAll(t, fc, c)
			assert.Equal(t, len(payload), rep.Totals.Converted)

			gens := make(map[string]string)

			for key, old := range before {
				sc, err := c.Stat(t.Context(), "b", key)
				require.NoError(t, err)

				assert.Equal(t, tc.to.String(), sc.Scheme, key)
				assert.Equal(t, old.ETag, sc.ETag, key)
				assert.Equal(t, old.Checksum, sc.Checksum, key)
				assert.True(t, old.Modified.Equal(sc.Modified), "conversion must not touch Modified: %s", key)
				assert.Equal(t, old.Seq+1, sc.Seq, key)
				assert.NotEqual(t, old.Generation, sc.Generation, key)

				gens[key] = sc.Generation
			}

			// Everything now lives exactly at the new scheme's placement — no
			// old-scheme fragments or sidecars anywhere.
			verifyPlacement(t, fc, tc.to, payload, gens)

			for key, want := range payload {
				assert.True(t, bytes.Equal(want, readObject(t, c, key)), "post-conversion read %s", key)
			}

			// Converged: a second scrub changes nothing.
			rep = scrubAll(t, fc, c)
			assert.Zero(t, rep.Repaired, "second pass must be a no-op")
			assert.Zero(t, rep.Totals.Converted)

			// New writes land at the new scheme directly.
			data := randBytes(2500)
			payload["fresh"] = data

			sc, err := c.Put(t.Context(), &PutRequest{
				Bucket: "b", Key: "fresh", Size: int64(len(data)), Body: bytes.NewReader(data),
			})
			require.NoError(t, err)
			assert.Equal(t, tc.to.String(), sc.Scheme)
		})
	}
}

func TestSchemeConversionViaRebalance(t *testing.T) {
	// The rebalance walk (CLI / admin API) converts too: set the scheme, run
	// one elected walk, done.
	from, to := scheme.Scheme{Kind: scheme.RF3}, scheme.Scheme{Kind: scheme.EC, K: 2, M: 1}

	fc := newFakeCluster(6, 1)
	c := fc.coordinator(t, Config{Scheme: fixedScheme(from)})

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	payload := putMany(t, c, 8)

	require.NoError(t, c.SetBucketScheme(t.Context(), "b", to.String()))

	r := rebalanceRunner(t, c, true)

	report, err := r.Rebalance(t.Context(), RebalanceOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.Failed)
	assert.Equal(t, len(payload), report.Totals.Converted)

	for key, want := range payload {
		sc, err := c.Stat(t.Context(), "b", key)
		require.NoError(t, err)
		assert.Equal(t, to.String(), sc.Scheme, key)
		assert.True(t, bytes.Equal(want, readObject(t, c, key)), key)
	}
}

func TestSetBucketSchemeValidation(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))

	// Unparseable scheme.
	require.Error(t, c.SetBucketScheme(t.Context(), "b", "rf9"))

	// A scheme the topology cannot host (ec:4,2 needs 6 targets, cluster has 3).
	require.ErrorIs(t, c.SetBucketScheme(t.Context(), "b", "ec:4,2"), ErrInsufficientTargets)

	// Unknown bucket.
	require.ErrorIs(t, c.SetBucketScheme(t.Context(), "nope", "rf3"), fs.ErrBucketNotFound)

	// Valid set, visible on the record; empty restores the default.
	require.NoError(t, c.SetBucketScheme(t.Context(), "b", "rf3"))

	info, err := c.Bucket(t.Context(), "b")
	require.NoError(t, err)
	assert.Equal(t, "rf3", info.Scheme)

	require.NoError(t, c.SetBucketScheme(t.Context(), "b", ""))

	info, err = c.Bucket(t.Context(), "b")
	require.NoError(t, err)
	assert.Empty(t, info.Scheme)
}

func TestEffectiveSchemeFallsBackWhenRecordUnreachable(t *testing.T) {
	// With the bucket record unreachable, writes proceed at the best-known
	// scheme instead of failing — and conversion never runs on a guess.
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	require.NoError(t, c.CreateBucket(t.Context(), "b", fs.ACLPrivate))
	require.NoError(t, c.SetBucketScheme(t.Context(), "b", "rf3"))

	for _, n := range fc.topo.Nodes {
		fc.setDown(n.ID, true)
	}

	// Cached (fresh) value serves.
	assert.Equal(t, "rf3", c.EffectiveScheme(t.Context(), "b").String())

	// Strict resolution refuses.
	_, err := c.freshBucketScheme(t.Context(), "other")
	require.Error(t, err)
}
