package memstore_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/memstore"
	"github.com/go-faster/fs/internal/cluster/metastore/metastoretest"
)

// TestConformance is the whole justification for this store: it is held to the
// same contract as every other backend, by the same suite, so a test that
// trusts it is trusting the contract rather than this implementation.
func TestConformance(t *testing.T) {
	metastoretest.Run(t, func(testing.TB) metastore.Store { return memstore.New() })
}

// TestScopeIsCluster pins what this backend answers, which is the reason it
// exists: cluster scope is the half of the metadata plane that has no other
// implementation until the sharded pebble plane lands.
func TestScopeIsCluster(t *testing.T) {
	assert.Equal(t, metastore.ScopeCluster, memstore.New().Scope())
}

// TestStartsUntrusted: a store that has not been built must refuse rather than
// answer short, and the safe default is what makes that happen without anyone
// remembering to arrange it.
func TestStartsUntrusted(t *testing.T) {
	state, err := memstore.New().State(t.Context())
	assert.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state)
}

// TestScanCallbackMayTouchTheStore covers the reason Scan snapshots under the
// lock and walks without it. The listing path calls back into the coordinator,
// which is exactly the shape that deadlocks a store holding its lock across the
// callback — and it would deadlock the test suite rather than fail it.
func TestScanCallbackMayTouchTheStore(t *testing.T) {
	store := memstore.New()

	for _, key := range []string{"a", "b"} {
		assert.NoError(t, store.Put(t.Context(), metastoretest.Entry("photos", key, 1, 1)))
	}

	err := store.Scan(t.Context(), "photos", "", "", 0, func(e metastore.Entry) error {
		_, _, err := store.Get(t.Context(), e.Bucket, e.Key)

		return err
	})
	assert.NoError(t, err)
}
