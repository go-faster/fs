package etcd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/metastore"
)

// planeState opens a watched flag against a fresh prefix.
func planeState(t *testing.T, client *clientv3.Client, prefix string) *etcd.PlaneState {
	t.Helper()

	p, err := etcd.NewPlaneState(t.Context(), client, etcd.Config{Prefix: prefix, TTL: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	return p
}

// state reads the flag, which never fails.
func state(t *testing.T, p *etcd.PlaneState) metastore.State {
	t.Helper()

	s, err := p.State(t.Context())
	require.NoError(t, err)

	return s
}

// eventually waits for the watch to deliver a state, which is the only way it
// ever arrives — Set does not touch the local value.
func eventually(t *testing.T, p *etcd.PlaneState, want metastore.State) {
	t.Helper()

	require.Eventually(t, func() bool { return state(t, p) == want },
		10*time.Second, 5*time.Millisecond)
}

// TestAFreshPlaneIsBuilding: an absent key is building. A plane nobody has ever
// marked ready has not been built, so a new cluster walks its sidecars until it
// has — slower, and right.
func TestAFreshPlaneIsBuilding(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/fresh")
	assert.Equal(t, metastore.StateBuilding, state(t, p))
}

// TestTheFlagIsClusterWide is the property the whole type exists for: a rebuild
// starting on one node is a rebuild for everyone.
//
// A node that believed its own share was ready would serve listings from a
// partitioning whose other ranges hold nothing, and a listing missing keys is a
// wrong answer where the sidecar walk is a right one.
func TestTheFlagIsClusterWide(t *testing.T) {
	client := startEtcd(t)

	writer := planeState(t, client, "/shared")
	reader := planeState(t, client, "/shared")

	require.NoError(t, writer.Set(t.Context(), metastore.Ready()))

	eventually(t, reader, metastore.StateReady)
	eventually(t, writer, metastore.StateReady)

	require.NoError(t, reader.Set(t.Context(), metastore.Building(metastore.CauseUnspecified)))

	eventually(t, writer, metastore.StateBuilding)
	eventually(t, reader, metastore.StateBuilding)
}

// TestAWriterLearnsThroughTheWatch: Set does not update the local value.
//
// It arrives the same way everyone else's does, so a writer and a reader on two
// nodes cannot disagree about when the change took effect — which they would if
// the writer counted from its own Put and the reader from the watch.
func TestAWriterLearnsThroughTheWatch(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/writer")

	require.NoError(t, p.Set(t.Context(), metastore.Ready()))
	eventually(t, p, metastore.StateReady)
}

// TestANewNodeSeesWhatWasAlreadyThere: the flag is loaded before the watch
// starts, so a node joining a ready cluster does not spend the first moments of
// its life reporting building.
func TestANewNodeSeesWhatWasAlreadyThere(t *testing.T) {
	client := startEtcd(t)

	first := planeState(t, client, "/joining")
	require.NoError(t, first.Set(t.Context(), metastore.Ready()))
	eventually(t, first, metastore.StateReady)

	joined := planeState(t, client, "/joining")
	assert.Equal(t, metastore.StateReady, state(t, joined),
		"answered from the load, before any event could have arrived")
}

// TestAnUnreadableFlagIsNotAPlane: a node that cannot read the flag at startup
// must not start.
//
// Defaulting to building would look conservative and be worse: the node would
// come up serving listings off the sidecar walk with nothing to tell an
// operator why, and the actual problem is that it has no control plane.
func TestAnUnreadableFlagIsNotAPlane(t *testing.T) {
	client := startEtcd(t)
	require.NoError(t, client.Close())

	_, err := etcd.NewPlaneState(t.Context(), client, etcd.Config{Prefix: "/dead", TTL: 2})
	require.Error(t, err)
}

// TestGarbageReadsAsBuilding: anything that is not exactly the ready marker is
// building, so a partially written or hand-edited value fails toward the
// correct-and-slow answer rather than toward serving from a plane nobody
// vouched for.
func TestGarbageReadsAsBuilding(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/garbage")
	require.NoError(t, p.Set(t.Context(), metastore.Ready()))
	eventually(t, p, metastore.StateReady)

	_, err := client.Put(t.Context(), "/garbage/metaplane/state", "READY")
	require.NoError(t, err)

	eventually(t, p, metastore.StateBuilding)
}

// TestDeletingTheFlagIsBuilding: the key going away is the same statement as it
// never having existed. Nobody currently vouches for this plane.
func TestDeletingTheFlagIsBuilding(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/deleted")
	require.NoError(t, p.Set(t.Context(), metastore.Ready()))
	eventually(t, p, metastore.StateReady)

	_, err := client.Delete(t.Context(), "/deleted/metaplane/state")
	require.NoError(t, err)

	eventually(t, p, metastore.StateBuilding)
}

// TestABrokenWatchReportsBuilding is the asymmetry the whole design turns on.
//
// The two directions of staleness are not equal. Believing the plane is
// building when it is ready costs a slower answer that is still correct;
// believing it is ready when a rebuild has started serves a listing missing
// keys, which is simply wrong. So a node that has lost its view of the flag
// reports building rather than the last thing it saw.
//
// The cost is real — an etcd outage puts every listing in the cluster back on
// the sidecar walk. That is the behavior this plane replaced, so it is degraded
// rather than broken, and it is the only direction worth failing in.
func TestABrokenWatchReportsBuilding(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/broken")
	require.NoError(t, p.Set(t.Context(), metastore.Ready()))
	eventually(t, p, metastore.StateReady)

	// Losing the client is losing the watch: nothing will deliver the next
	// change, so the last value seen stops being evidence of anything.
	require.NoError(t, client.Close())

	eventually(t, p, metastore.StateBuilding)
}

// TestCloseIsIdempotent: a node shutting down closes what it opened, and a
// second close must not panic on an already-canceled watch.
func TestCloseIsIdempotent(t *testing.T) {
	client := startEtcd(t)

	p, err := etcd.NewPlaneState(t.Context(), client, etcd.Config{Prefix: "/closing", TTL: 2})
	require.NoError(t, err)

	require.NoError(t, p.Close())
	require.NoError(t, p.Close())
}

// raw publishes a value written by something other than Set — an operator, or a
// version that named its causes differently — and waits for the watch to deliver
// it.
//
// The plane is marked ready first, and that is the whole point of the helper. A
// fresh prefix already reads as building, so waiting for "building" after
// writing a building value returns before the watch has delivered anything, and
// the cause read afterwards is whatever was there before. Every one of these
// tests passed that way while asserting nothing.
func raw(t *testing.T, client *clientv3.Client, p *etcd.PlaneState, prefix, value string) {
	t.Helper()

	require.NoError(t, p.Set(t.Context(), metastore.Ready()))
	eventually(t, p, metastore.StateReady)

	_, err := client.Put(t.Context(), prefix+"/metaplane/state", value)
	require.NoError(t, err)

	eventually(t, p, metastore.StateBuilding)
}

// status reads the flag with its cause.
func status(t *testing.T, p *etcd.PlaneState) metastore.Build {
	t.Helper()

	b, err := p.Status(t.Context())
	require.NoError(t, err)

	return b
}

// TestTheCauseTravelsWithTheFlag: the reason a plane is building is what decides
// whether a rebuild starts without an operator, and it has to reach every node
// the flag does.
func TestTheCauseTravelsWithTheFlag(t *testing.T) {
	client := startEtcd(t)

	writer := planeState(t, client, "/cause")
	reader := planeState(t, client, "/cause")

	require.NoError(t, writer.Set(t.Context(), metastore.Building(metastore.CauseOrphaned)))
	eventually(t, reader, metastore.StateBuilding)

	require.Eventually(t, func() bool {
		return status(t, reader).Cause == metastore.CauseOrphaned
	}, 10*time.Second, 5*time.Millisecond)
}

// TestAReadyPlaneCarriesNoCause: the flag and the reason are one value, so a
// node cannot see "building" from one revision and a cause from another — which
// is what a second key would allow, and which would have a rebuild start on the
// reason for a state the cluster had already left.
func TestAReadyPlaneCarriesNoCause(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/atomic")

	require.NoError(t, p.Set(t.Context(), metastore.Building(metastore.CauseOrphaned)))
	eventually(t, p, metastore.StateBuilding)

	require.NoError(t, p.Set(t.Context(), metastore.Ready()))
	eventually(t, p, metastore.StateReady)

	got := status(t, p)
	assert.Equal(t, metastore.StateReady, got.State)
	assert.Equal(t, metastore.CauseUnspecified, got.Cause,
		"a ready plane carried the reason it used to be unready")
}

// TestAnOlderMarkerReadsAsBuildingWithNoCause: a cluster mid-upgrade has nodes
// writing "building" without a reason. A newer node must read that as building —
// which it always did — and as no grounds to start hours of I/O.
func TestAnOlderMarkerReadsAsBuildingWithNoCause(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/older")

	raw(t, client, p, "/older", "building")
	assert.Equal(t, metastore.CauseUnspecified, status(t, p).Cause)
}

// TestABareCauseNameIsNotACause: the reason is only a reason when it arrives in
// the shape this writes.
//
// The case is an operator putting the key by hand — "orphaned", say, meaning to
// describe rather than to instruct. Read loosely that is a command to walk every
// disk in the cluster. Read as this does, it is a value nobody recognizes, which
// is building with no cause and no rebuild.
func TestABareCauseNameIsNotACause(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/bare")

	raw(t, client, p, "/bare", "orphaned")
	assert.Equal(t, metastore.CauseUnspecified, status(t, p).Cause,
		"a hand-written note read as an instruction to rebuild")
}

// TestAnUnknownCauseReadsAsUnspecified: a value written by a version that names
// its causes differently must not be guessed at.
func TestAnUnknownCauseReadsAsUnspecified(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/unknown")

	raw(t, client, p, "/unknown", "building:something-else")
	assert.Equal(t, metastore.CauseUnspecified, status(t, p).Cause)
}

// TestTheFlagStaysReadableByHand: an operator reads this key with etcdctl while
// deciding whether a cluster is healthy, and "1" is not an answer to that
// question.
func TestTheFlagStaysReadableByHand(t *testing.T) {
	client := startEtcd(t)

	p := planeState(t, client, "/readable")

	require.NoError(t, p.Set(t.Context(), metastore.Building(metastore.CauseOrphaned)))

	resp, err := client.Get(t.Context(), "/readable/metaplane/state")
	require.NoError(t, err)
	require.Len(t, resp.Kvs, 1)

	assert.Equal(t, "building:orphaned", string(resp.Kvs[0].Value))
}
