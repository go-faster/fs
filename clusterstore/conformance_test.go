package clusterstore

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/storagetest"
)

// TestConformance runs the exported fs.Storage conformance suite against the
// cluster storage — every guarantee of the single-node backends must hold
// over the replicated data plane, for each replication scheme.
func TestConformance(t *testing.T) {
	for _, s := range []scheme.Scheme{
		{Kind: scheme.RF25},
		{Kind: scheme.RF3},
		{Kind: scheme.EC, K: 2, M: 1},
		{Kind: scheme.EC, K: 4, M: 2},
	} {
		t.Run(s.String(), func(t *testing.T) {
			storagetest.Run(t, func(tb testing.TB) fs.Storage {
				fc := newFakeCluster(6, 2)

				c, err := New(Config{
					Topology: StaticTopology{T: fc.topo},
					Peers:    fc,
					Scheme:   fixedScheme(s),
				})
				require.NoError(tb, err)
				tb.Cleanup(func() { _ = c.Close() })

				return NewStorage(c)
			})
		})
	}
}
