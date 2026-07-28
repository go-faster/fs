package objindex_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/metastore/metastoretest"
	"github.com/go-faster/fs/internal/cluster/objindex"
)

// TestConformance runs the shared metastore.Store suite. Everything a caller
// may rely on lives there, so that the PostgreSQL backend and this one are
// held to one contract rather than to whichever tests each grew.
func TestConformance(t *testing.T) {
	metastoretest.Run(t, func(t testing.TB) metastore.Store { return open(t) })
}

// open returns a fresh index under a temp dir, closed at the end of the test.
func open(t testing.TB) *objindex.Index {
	t.Helper()

	idx, err := objindex.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	return idx
}

// entry is the suite's fixture, so the tests below order records the same way
// the conformance cases do.
var entry = metastoretest.Entry

// TestScopeIsLocal pins what this backend in particular answers. The suite only
// checks that a store declares one of the two scopes; which one is a property
// of the implementation, and this index describes one node's disks.
func TestScopeIsLocal(t *testing.T) {
	assert.Equal(t, metastore.ScopeLocal, open(t).Scope())
}

// TestStateRoundTrip covers the contract that makes unsynced writes safe, and
// it is pebble-specific because it is about this backend's own recovery: an
// index is usable only after a build, and only a clean handover carries that
// across a restart. A cluster-scope store answers the same question in an
// entirely different way, which is why it is not in the shared suite.
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	first, err := objindex.Open(dir)
	require.NoError(t, err)

	state, err := first.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state, "a fresh index has nothing in it")

	require.NoError(t, first.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))
	require.NoError(t, first.MarkReady(t.Context()))
	require.NoError(t, first.Close())

	second, err := objindex.Open(dir)
	require.NoError(t, err)

	state, err = second.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state,
		"opening invalidates: only the next clean close may call it ready again")

	got, found, err := second.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	require.True(t, found, "the entries are still there, they are just not trusted yet")
	assert.Equal(t, int64(100), got.Size)

	require.NoError(t, second.MarkReady(t.Context()))
	require.NoError(t, second.Close())

	third, err := objindex.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = third.Close() })

	// Close marked it ready, Open marked it building again — the point is that
	// a process which never closes leaves it building, which is what schedules
	// the rebuild.
	state, err = third.State(t.Context())
	require.NoError(t, err)
	assert.Equal(t, metastore.StateBuilding, state)
}

// TestSyncWritesOption is a durability knob on this backend, not part of the
// interface: what it buys is that an acknowledged object write is never missing
// from the index, at the cost of a WAL fsync per object.
func TestSyncWritesOption(t *testing.T) {
	idx, err := objindex.Open(t.TempDir(), objindex.WithSyncWrites(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	require.NoError(t, idx.Put(t.Context(), entry("photos", "a.jpg", 100, 1)))

	got, found, err := idx.Get(t.Context(), "photos", "a.jpg")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(100), got.Size)
}

// TestDirIsWhereItWasOpened: Dir is deliberately not on metastore.Store — a
// store backed by anything other than a local database has no directory to
// name — so it is covered here or nowhere.
func TestDirIsWhereItWasOpened(t *testing.T) {
	dir := t.TempDir()

	idx, err := objindex.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Close() })

	assert.Equal(t, dir, idx.Dir())
}

// TestCloseIsIdempotent: Close is registered as a cluster shutdown hook and the
// teardown path can be reached twice, from two goroutines, on the construction
// error paths as well as the ordinary one.
func TestCloseIsIdempotent(t *testing.T) {
	idx, err := objindex.Open(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, idx.Close())
	require.NoError(t, idx.Close())
}
