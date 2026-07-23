//go:build chaos

package chaos

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putDeadline bounds how long a write retries across live nodes before it is
// counted a failure. It must exceed the worst-case single-node restart window
// (bounded by healthTimeout) so a write issued while one node is briefly down
// retries through the window rather than failing — the essence of a
// zero-failed-request rolling upgrade.
const putDeadline = healthTimeout + 60*time.Second

// putResilient PUTs through any live node, retrying across all of them until
// one acks or the deadline passes. Returns nil on success.
func putResilient(ctx context.Context, clients []*minio.Client, bucket, key string, data []byte) error {
	deadline := time.Now().Add(putDeadline)

	for {
		for _, c := range clients {
			opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := c.PutObject(opCtx, bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})

			cancel()

			if err == nil {
				return nil
			}

			if ctx.Err() != nil {
				return ctx.Err()
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("put %s/%s: no node acked within %s", bucket, key, putDeadline)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// TestRollingUpgrade is the ROADMAP Phase 10 acceptance drill: a 3-node
// cluster upgraded in place node-by-node under continuous load, with zero
// failed requests. Each node is stopped GRACEFULLY (SIGTERM → drain →
// deregister), restarted (standing in for the new binary), and the cluster is
// let converge to full protection before the next node is taken down — never
// more than one node down at a time.
//
// Invariants asserted:
//   - zero failed requests: every PUT and GET, retried across live nodes,
//     ultimately succeeds (a write issued during a node-down window retries
//     through it);
//   - no data loss: every acked write reads back correctly after the upgrade;
//   - convergence: the cluster returns to full protection between steps.
func TestRollingUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("rolling-upgrade drill is long-running")
	}

	e := startEtcd(t)
	ctx := t.Context()

	// Node addresses are assigned once and stable across restarts, so a fixed
	// client set stays valid throughout (no node is added or removed).
	nodes := make([]*node, 3)
	for i := range nodes {
		nodes[i] = newNode(t, i, e.clientURL.String())
		nodes[i].start(t)
	}

	for _, n := range nodes {
		n.waitHealthy(t)
	}

	clients := s3Clients(t, nodes)

	coord, verifier := newVerifier(t, e.clientURL.String())

	require.Eventually(t, func() bool {
		return coord.Topology().DiskCount() == len(nodes)
	}, 30*time.Second, 50*time.Millisecond, "cluster must converge before the drill")

	for _, bucket := range chaosBuckets {
		require.NoError(t, clients[0].MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))
	}

	nodes[0].runCLI(t, "cluster", "scheme", "b-rf3", "rf3")
	nodes[0].runCLI(t, "cluster", "scheme", "b-ec", "ec:2,1")

	// Continuous load with a zero-tolerance failure counter. Each writer owns
	// its keys so the last acked payload per key is deterministic.
	lg := newLedger()

	var putFailures, getFailures atomic.Int64

	// paused quiesces the writers while the between-step convergence check
	// runs: under continuous load a handful of just-acked objects always have
	// an async remainder in flight, so "0 misplaced" is only observable when
	// new writes briefly stop. The risky work (stop/restart/rejoin) still runs
	// under full load.
	var paused atomic.Bool

	loadCtx, stopLoad := context.WithCancel(ctx)

	var writers sync.WaitGroup

	const (
		writerCount   = 4
		keysPerWriter = 12
	)

	for w := range writerCount {
		writers.Go(func() {
			rnd := rand.New(rand.NewSource(int64(w)))

			for i := 0; loadCtx.Err() == nil; i++ {
				if paused.Load() {
					time.Sleep(20 * time.Millisecond)
					continue
				}

				bucket := chaosBuckets[i%len(chaosBuckets)]
				key := fmt.Sprintf("w%d-k%d", w, rnd.Intn(keysPerWriter))

				data := make([]byte, 1024+rnd.Intn(4096))
				rnd.Read(data)

				sum := md5.Sum(data)
				idx := lg.begin(bucket, key, hex.EncodeToString(sum[:]))

				if err := putResilient(loadCtx, clients, bucket, key, data); err != nil {
					if loadCtx.Err() == nil {
						putFailures.Add(1)
						t.Errorf("PUT failed during rolling upgrade: %v", err)
					}

					continue
				}

				lg.ack(bucket, key, idx)

				// Read it back through any node; a read must never fail while
				// the object is durable at W=2 on the surviving nodes.
				if _, err := readAnyHash(loadCtx, clients, bucket, key); err != nil && loadCtx.Err() == nil {
					getFailures.Add(1)
					t.Errorf("GET failed during rolling upgrade: %v", err)
				}
			}
		})
	}

	// Warm up before the first step.
	time.Sleep(5 * time.Second)

	// Roll through every node, one at a time.
	for i, n := range nodes {
		t.Logf("rolling upgrade: node %s (%d/%d)", n.id, i+1, len(nodes))

		n.stopGraceful(t)

		// Graceful stop deregisters, so the topology drops promptly (not after
		// the lease TTL). This confirms the drain path ran.
		require.Eventually(t, func() bool {
			return coord.Topology().DiskCount() == len(nodes)-1
		}, 30*time.Second, 50*time.Millisecond, "node %s must leave the topology on graceful stop", n.id)

		// Restart (the "new binary") and wait for it to rejoin.
		n.start(t)
		n.waitHealthy(t)

		require.Eventually(t, func() bool {
			return coord.Topology().DiskCount() == len(nodes)
		}, healthTimeout, 100*time.Millisecond, "node %s must rejoin", n.id)

		// Converge to full protection before taking the next node down —
		// otherwise a pre-existing object still missing the fragment that lived
		// on this node would be under-protected when the next node stops (and,
		// for EC, a second missing shard is unrecoverable). The nodes' own
		// scrubs restore it; the verifier only observes. Quiesce new writes for
		// the check so transient in-flight remainders don't mask convergence.
		paused.Store(true)

		require.Eventually(t, func() bool {
			plan, err := verifier.PlanRebalance(ctx)
			return err == nil && plan.MisplacedObjects == 0 && plan.Unplannable == 0
		}, 2*time.Minute, 2*time.Second, "cluster must reconverge after node %s before the next step", n.id)

		paused.Store(false)
	}

	stopLoad()
	writers.Wait()

	// Zero failed requests.
	assert.Zero(t, putFailures.Load(), "rolling upgrade must not fail any write")
	assert.Zero(t, getFailures.Load(), "rolling upgrade must not fail any read")

	// No data loss: every acked write reads back as its last-acked payload.
	keys := lg.ackedKeys()
	require.NotEmpty(t, keys, "the load generator must have acked writes")

	t.Logf("rolling upgrade: verifying %d acked keys", len(keys))

	for _, bk := range keys {
		lg.verify(t, bk[0], bk[1], func() (string, error) {
			return readAnyHash(ctx, clients, bk[0], bk[1])
		})
	}
}
