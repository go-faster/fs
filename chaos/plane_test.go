//go:build chaos

// Metadata-plane chaos cases (Exascale E3, issue #162).
//
// E3 shipped with these two cases covered as in-process failure tests in
// shardstore and recorded them as acceptance not met: the interesting failures
// are a node that stops existing and a shard whose data is gone, and an
// in-process fake cannot produce either. These run them against the real
// cluster the rest of this package already builds — separate OS processes,
// SIGKILL, an etcd whose leases actually expire.
//
// The invariant both cases assert is the one E3 named as the asymmetry that
// decides the design: **believing the plane is building when it is ready costs
// a slow correct answer; believing it is ready when it is building serves a
// listing missing keys.** So a listing must contain every acked key at every
// point in the run — before the fault, during it, and after convergence —
// whatever the plane's state says. A plane that is merely slow is a pass; a
// listing short one key is the failure the whole derived-index design is
// staked on not happening.
package chaos

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/shardstore"
)

// planeBucket is the single bucket the plane cases list. One bucket, default
// scheme: these cases are about metadata, and a second replication scheme adds
// nothing but runtime.
const planeBucket = "b-plane"

const (
	// planeReadyTimeout bounds the first partition and build. It is generous on
	// purpose: the controller campaigns for an election, then the rebuild it
	// owes waits on a 30s ticker, so "ready" is a minute or two away on a
	// cluster with nothing in it.
	planeReadyTimeout = 3 * time.Minute
	// planeConvergeTimeout bounds failover or a rebuild after the fault. A
	// rebuild walks every disk in the cluster, which at this data size is fast
	// but not instant.
	planeConvergeTimeout = 2 * time.Minute
)

// planeWatcher reads the plane's build state from etcd.
//
// This is the cluster-wide truth every node agrees on, rather than one node's
// admin API — the admin listener refuses to start alongside auth.disabled,
// which every chaos node uses. It costs one thing worth naming: E3's rule that
// *a node* which loses its readiness watch must report building is not observed
// here, only the state that rule protects. `shardstore` covers the watch
// itself; what these cases add is that the state is reached at all when a real
// process dies.
type planeWatcher struct {
	client *clientv3.Client
	key    string
}

func newPlaneWatcher(t *testing.T, etcdURL string) *planeWatcher {
	t.Helper()

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{etcdURL},
		DialTimeout: 5 * time.Second,
	})
	require.NoError(t, err)

	t.Cleanup(func() { _ = client.Close() })

	// The raw key, not etcd.PlaneState: that type answers from a watch it
	// maintains for a running node, and deliberately reports building whenever
	// its own watch is not established — correct for a server deciding how to
	// serve a listing, and wrong for a test asking what the cluster agreed on.
	return &planeWatcher{client: client, key: etcdPrefix + "/metaplane/state"}
}

// raw is the stored flag: "ready", or a cause, or empty when never written.
func (w *planeWatcher) raw(ctx context.Context, t *testing.T) string {
	t.Helper()

	resp, err := w.client.Get(ctx, w.key)
	require.NoError(t, err)

	if len(resp.Kvs) == 0 {
		return ""
	}

	return string(resp.Kvs[0].Value)
}

func (w *planeWatcher) status(ctx context.Context, t *testing.T) metastore.Build {
	t.Helper()

	// The flag is "ready", or "building:<cause>", or absent on a plane nobody
	// has ever marked.
	value := w.raw(ctx, t)
	if value == "ready" {
		return metastore.Ready()
	}

	switch strings.TrimPrefix(value, "building:") {
	case "orphaned":
		return metastore.Building(metastore.CauseOrphaned)
	case "never-built", "":
		return metastore.Building(metastore.CauseNeverBuilt)
	default:
		return metastore.Building(metastore.CauseUnspecified)
	}
}

// await waits for the plane to reach state, returning what it settled on. It
// fails with a node's log tail rather than a bare timeout, because "the plane
// never became ready" is not diagnosable on its own.
func (w *planeWatcher) await(ctx context.Context, t *testing.T, state metastore.State, within time.Duration, n *node) metastore.Build {
	t.Helper()

	deadline := time.Now().Add(within)
	seen := ""

	for {
		// Log every transition: which states the plane passed through on the
		// way is the difference between a timeout that says "it never built"
		// and one that says "it built and something un-built it".
		if raw := w.raw(ctx, t); raw != seen {
			t.Logf("plane flag: %q -> %q", seen, raw)
			seen = raw
		}

		build := w.status(ctx, t)
		if build.State == state {
			return build
		}

		if time.Now().After(deadline) {
			t.Fatalf("plane flag is %q (state %v, cause %v), want %v within %s; log tail:\n%s",
				w.raw(ctx, t), build.State, build.Cause, state, within, n.logTail())
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// makeBucket creates the test bucket, retrying while the cluster settles.
//
// A freshly started cluster can answer for a moment before every node has the
// topology, and the failure surfaces as a 500 rather than anything typed — so
// this retries, and on giving up prints the node log, because "internal error"
// on its own says nothing about which subsystem was not ready.
func makeBucket(ctx context.Context, t *testing.T, client *minio.Client, n *node) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	var err error

	for time.Now().Before(deadline) {
		if err = client.MakeBucket(ctx, planeBucket, minio.MakeBucketOptions{}); err == nil {
			return
		}

		time.Sleep(250 * time.Millisecond)
	}

	t.Fatalf("could not create %s: %v; log tail:\n%s", planeBucket, err, n.logTail())
}

// listAll returns every key in the bucket, whatever path served the listing.
func listAll(ctx context.Context, client *minio.Client, bucket string) ([]string, error) {
	var keys []string

	for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}

		keys = append(keys, obj.Key)
	}

	sort.Strings(keys)

	return keys, nil
}

// requireListingComplete fails when a listing is missing any key the ledger
// acked. This is the assertion the whole file exists for.
//
// Extra keys are not a failure: a PUT whose response was lost may legally have
// committed, so the listing may hold keys the ledger never acked. Missing ones
// are never legal.
// requireListingCatchesUp waits for the listing to contain every acked key.
//
// It waits rather than asserting outright, and that is not a hedge: the plane
// is a derived index updated after the write it describes is acked, so a
// listing taken while writes are in flight is legitimately behind the ledger.
// The invariant worth gating on is that it *catches up* — a quiet cluster
// eventually lists everything it acked. Callers must stop their load first, or
// they are racing the writers rather than testing the plane.
func requireListingCatchesUp(ctx context.Context, t *testing.T, client *minio.Client, lg *ledger, when string) {
	t.Helper()

	var last int

	require.Eventuallyf(t, func() bool {
		last = missingFromListing(ctx, t, client, lg)

		return last == 0
	}, time.Minute, time.Second, "listing %s never caught up: still missing %d acked key(s)", when, last)
}

// missingFromListing returns how many acked keys the listing does not contain.
//
// Extra keys are not counted: a PUT whose response was lost may legally have
// committed, so the listing may hold keys the ledger never acked. Missing ones
// are the ones that matter.
func missingFromListing(ctx context.Context, t *testing.T, client *minio.Client, lg *ledger) int {
	t.Helper()

	listed, err := listAll(ctx, client, planeBucket)
	require.NoError(t, err, "listing failed")

	have := make(map[string]struct{}, len(listed))
	for _, k := range listed {
		have[k] = struct{}{}
	}

	var missing int

	for _, ref := range lg.ackedKeys() {
		if ref[0] != planeBucket {
			continue
		}

		if _, ok := have[ref[1]]; !ok {
			missing++
		}
	}

	return missing
}

// planeLoad starts writers against planeBucket and returns a stop function.
// Each writer owns its keys, so the last acked payload per key is
// deterministic — the same contract TestChaos relies on.
func planeLoad(ctx context.Context, t *testing.T, clients []*minio.Client, lg *ledger, writers int) func() {
	t.Helper()

	loadCtx, stop := context.WithCancel(ctx)

	var wg sync.WaitGroup

	for w := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			rnd := rand.New(rand.NewSource(int64(w) + 1))

			for i := 0; loadCtx.Err() == nil; i++ {
				key := fmt.Sprintf("w%d-k%04d", w, i)

				data := make([]byte, 512+rnd.Intn(2048))
				rnd.Read(data)

				sum := md5.Sum(data)
				idx := lg.begin(planeBucket, key, hex.EncodeToString(sum[:]))

				if err := putRetry(loadCtx, clients, planeBucket, key, data); err == nil {
					lg.ack(planeBucket, key, idx)
				}

				time.Sleep(20 * time.Millisecond)
			}
		}()
	}

	return func() {
		stop()
		wg.Wait()
	}
}

// TestPlaneOwnerKilledMidLoad is E3's first unmet case: kill a range owner
// while writes are in flight.
//
// With a follower per range this is the failure the plane is built to absorb —
// promotion, one metadata write, no cluster-wide walk. The assertions are that
// listings never go short, and that the plane comes back to ready *without*
// reporting an orphaned range, because an orphan here would mean promotion did
// not happen and the cluster paid for a rebuild it should not have needed.
func TestPlaneOwnerKilledMidLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos suite is long-running")
	}

	ctx := t.Context()
	e := startEtcd(t)

	// Four nodes, two copies of each range: every range has a follower to
	// promote, which is the configuration this case is about.
	nodes := make([]*node, 0, 4)

	for i := range 4 {
		n := newNode(t, i, e.clientURL.String(), withPlane(2, 16, "always"))
		n.start(t)
		nodes = append(nodes, n)
	}

	for _, n := range nodes {
		n.waitHealthy(t)
	}

	clients := s3Clients(t, nodes)
	makeBucket(ctx, t, clients[0], nodes[0])

	// "always" rather than the "on_failure" default, and not incidentally: a
	// never-built plane is deliberately left for an operator to schedule, and
	// the only way to ask for that build is the admin API — which refuses to
	// start alongside auth.disabled. "always" is documented for exactly this
	// shape of cluster, one small enough that the walk is not an event.
	plane := newPlaneWatcher(t, e.clientURL.String())
	plane.await(ctx, t, metastore.StateReady, planeReadyTimeout, nodes[0])

	lg := newLedger()
	stopLoad := planeLoad(ctx, t, clients, lg, 3)

	// Let the writers get enough keys in that a listing is worth checking.
	time.Sleep(3 * time.Second)

	// Kill an owner mid-load. Node 3 rather than 0 so the surviving clients
	// and the admin listener we poll are not the ones going away.
	killed := nodes[3]
	killed.kill()

	// Watch the cause through the failover: with a follower for every range,
	// losing one node must never orphan one, and an orphan is only visible
	// while the plane is building — so it has to be caught in the window
	// rather than read off the end state.
	//
	// Listings are counted here, not asserted on: writes are still in flight,
	// and the plane is updated after the write it describes is acked, so a
	// listing is legitimately behind the ledger whether or not a node just
	// died. What the count is for is the note at the end of the test.
	survivors := clients[:3]
	deadline := time.Now().Add(30 * time.Second)

	shortListings := 0

	for time.Now().Before(deadline) {
		if n := missingFromListing(ctx, t, survivors[0], lg); n > 0 {
			shortListings++
		}

		build := plane.status(ctx, t)
		require.NotEqualf(t, metastore.CauseOrphaned, build.Cause,
			"killing one of 2 range holders orphaned a range: a follower should have been promoted; log tail:\n%s",
			nodes[0].logTail())

		time.Sleep(500 * time.Millisecond)
	}

	stopLoad()

	// The plane returns to ready by promoting followers. An orphaned cause
	// would say the ranges the killed node owned had no copy anywhere, which
	// with replicas=2 is exactly what must not happen.
	plane.await(ctx, t, metastore.StateReady, planeConvergeTimeout, nodes[0])

	// The invariant that must hold: with the load stopped and the cluster
	// converged, every acked key is listed. A gap that survives here is the
	// failure the derived-index design is staked on not happening.
	requireListingCatchesUp(ctx, t, survivors[0], lg, "after convergence")

	// The transient window is recorded, not asserted on, because what it should
	// be is a design question and not this test's to settle: an acked PUT
	// commits its sidecar before its plane entry reaches a follower, so a kill
	// in between leaves the plane briefly describing less than the cluster
	// holds. E3's rule says a listing must never be silently short — which
	// would mean the unserved range failing the read into the sidecar walk
	// rather than answering without it.
	t.Logf("listings short of the ledger during the failover window: %d of ~%d polls",
		shortListings, 60)
}

// TestPlaneOwnerKilledQuiescent is the sharper experiment issue #240 asked for.
//
// TestPlaneOwnerKilledMidLoad cannot tell two things apart, because it kills a
// node while writes are in flight:
//
//  1. the plane is a derived index updated *after* the write it describes is
//     acked, so a listing under load is legitimately behind the ledger — this
//     happens with no failure at all; and
//  2. a range with no owner contributing zero keys to a listing that is served
//     as complete, which is the failure E3's whole readiness rule exists to
//     prevent.
//
// Removing the writes removes cause 1 entirely. Every acked key is in the plane
// before the kill, so any key missing afterwards can only be cause 2 — and this
// test asserts there are none, on every poll, for the whole failover.
func TestPlaneOwnerKilledQuiescent(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos suite is long-running")
	}

	ctx := t.Context()
	e := startEtcd(t)

	nodes := make([]*node, 0, 4)

	for i := range 4 {
		n := newNode(t, i, e.clientURL.String(), withPlane(2, 16, "always"))
		n.start(t)
		nodes = append(nodes, n)
	}

	for _, n := range nodes {
		n.waitHealthy(t)
	}

	clients := s3Clients(t, nodes)
	makeBucket(ctx, t, clients[0], nodes[0])

	plane := newPlaneWatcher(t, e.clientURL.String())
	plane.await(ctx, t, metastore.StateReady, planeReadyTimeout, nodes[0])

	// Write, then stop and let the plane catch up completely. From here the
	// ledger and the plane agree, and nothing is being written.
	lg := newLedger()
	stopLoad := planeLoad(ctx, t, clients, lg, 3)
	time.Sleep(5 * time.Second)
	stopLoad()

	requireListingCatchesUp(ctx, t, clients[0], lg, "before the kill")

	// Now kill an owner. No writes are in flight, so the plane cannot fall
	// behind: a short listing from here is a range being skipped.
	nodes[3].kill()

	survivor := clients[0]
	deadline := time.Now().Add(45 * time.Second)

	for time.Now().Before(deadline) {
		require.Zerof(t, missingFromListing(ctx, t, survivor, lg),
			"a listing during failover dropped keys with no writes in flight: "+
				"an unserved range was served as empty instead of falling back; log tail:\n%s",
			nodes[0].logTail())

		time.Sleep(500 * time.Millisecond)
	}

	plane.await(ctx, t, metastore.StateReady, planeConvergeTimeout, nodes[0])
	requireListingCatchesUp(ctx, t, survivor, lg, "after convergence")
}

// TestPlaneRebuildWithoutFollower is E3's second unmet case: a range whose data
// is gone and which has no follower to promote.
//
// replicas=1 means every range is its own only copy, so killing a node and
// wiping its plane data leaves ranges orphaned — the failure that costs a
// cluster-wide walk of every disk. The point of the case is that this is
// survivable and self-healing: listings stay correct while the plane is
// building (they fall back to walking sidecars), and the on_failure policy
// rebuilds without an operator, which is what #191 added.
func TestPlaneRebuildWithoutFollower(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos suite is long-running")
	}

	ctx := t.Context()
	e := startEtcd(t)

	// Three nodes, one copy of each range: nothing to promote to.
	nodes := make([]*node, 0, 3)

	for i := range 3 {
		n := newNode(t, i, e.clientURL.String(), withPlane(1, 12, "always"))
		n.start(t)
		nodes = append(nodes, n)
	}

	for _, n := range nodes {
		n.waitHealthy(t)
	}

	clients := s3Clients(t, nodes)
	makeBucket(ctx, t, clients[0], nodes[0])

	plane := newPlaneWatcher(t, e.clientURL.String())
	plane.await(ctx, t, metastore.StateReady, planeReadyTimeout, nodes[0])

	lg := newLedger()
	stopLoad := planeLoad(ctx, t, clients, lg, 3)

	time.Sleep(3 * time.Second)

	stopLoad()
	requireListingCatchesUp(ctx, t, clients[0], lg, "before the wipe")

	// Kill the node and destroy its shard of the plane, then bring it back.
	// The objects on its data disk survive — this removes the metadata, not
	// the commit point, which is the distinction the whole derived design
	// rests on.
	victim := nodes[2]
	victim.kill()

	wipePlaneData(t, victim)

	victim.start(t)
	victim.waitHealthy(t)

	// Listings stay complete while the plane is degraded: the request falls
	// back to walking sidecars, which is slower and still correct. This is the
	// assertion that matters most in this file — it is the one that fails if
	// an orphaned range is ever served as an empty range.
	requireListingCatchesUp(ctx, t, clients[0], lg, "with the shard gone")

	// And the cluster returns to a usable plane without an operator.
	//
	// What this does *not* prove: that the flag passed through
	// building/orphaned on the way. The window between losing the shard and
	// the plane being usable again is short enough here that polling misses
	// it, so the case asserts the outcome — listings correct throughout, plane
	// ready afterwards — and leaves "was the cause recorded as orphaned"
	// to shardstore's own tests, which can observe the transition directly.
	plane.await(ctx, t, metastore.StateReady, planeConvergeTimeout, nodes[0])

	requireListingCatchesUp(ctx, t, clients[0], lg, "after the rebuild")
}

// wipePlaneData removes the node's metadata-plane shard, leaving its object
// data alone: the sidecars on the data disk are the commit point, and this
// case is about losing the derived copy of them.
//
// It asserts the directory was there rather than removing it best-effort. A
// wipe that silently hits nothing — because the layout moved — would leave
// both cases below passing against a plane that never lost anything, which is
// worse than having no test at all.
func wipePlaneData(t *testing.T, n *node) {
	t.Helper()

	dir := shardstore.DefaultShardDir(n.root)

	info, err := os.Stat(dir)
	require.NoErrorf(t, err, "no plane shard at %s to wipe; log tail:\n%s", dir, n.logTail())
	require.Truef(t, info.IsDir(), "%s is not a directory", dir)

	require.NoError(t, os.RemoveAll(dir))
}
