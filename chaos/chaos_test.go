//go:build chaos

package chaos

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// buckets under test, one per replication scheme.
var chaosBuckets = []string{"b-rf25", "b-rf3", "b-ec"}

// attempt is one issued PUT for a key.
type attempt struct {
	hash  string
	acked bool
}

// ledger tracks every issued write per bucket/key. The durability contract it
// checks: once a PUT is acked, a read of the key returns the payload of that
// PUT or of a later issued one (an unacked later PUT may legally have
// committed before its response was lost) — never anything older, never a
// miss.
type ledger struct {
	mu   sync.Mutex
	keys map[string][]attempt // bucket/key → attempts in issue order
}

func newLedger() *ledger { return &ledger{keys: make(map[string][]attempt)} }

func (l *ledger) ref(bucket, key string) string { return bucket + "/" + key }

// begin registers an issued PUT and returns its index.
func (l *ledger) begin(bucket, key, hash string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	ref := l.ref(bucket, key)
	l.keys[ref] = append(l.keys[ref], attempt{hash: hash})

	return len(l.keys[ref]) - 1
}

// ack marks an issued PUT acknowledged.
func (l *ledger) ack(bucket, key string, idx int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.keys[l.ref(bucket, key)][idx].acked = true
}

// verify checks one key's read result against the contract; keys with no
// acked write are skipped (nothing is guaranteed about them).
func (l *ledger) verify(t *testing.T, bucket, key string, read func() (string, error)) {
	t.Helper()

	l.mu.Lock()
	attempts := append([]attempt(nil), l.keys[l.ref(bucket, key)]...)
	l.mu.Unlock()

	lastAcked := -1

	for i, a := range attempts {
		if a.acked {
			lastAcked = i
		}
	}

	if lastAcked < 0 {
		return
	}

	hash, err := read()
	require.NoError(t, err, "acked object %s/%s must be readable", bucket, key)

	for _, a := range attempts[lastAcked:] {
		if a.hash == hash {
			return
		}
	}

	t.Errorf("%s/%s: read %s, want the last acked write (%s) or a later issued one", bucket, key, hash, attempts[lastAcked].hash)
}

// dump logs a key's attempt history, marking the attempt matching checksum —
// it answers whether a stuck generation was ever acknowledged.
func (l *ledger) dump(t *testing.T, bucket, key, checksum string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, a := range l.keys[l.ref(bucket, key)] {
		marker := ""
		if a.hash == checksum {
			marker = "  <== stuck generation"
		}

		t.Logf("diagnose: %s/%s attempt %d: hash %s acked %v%s", bucket, key, i, a.hash, a.acked, marker)
	}
}

// ackedKeys lists every bucket/key with at least one acknowledged write.
func (l *ledger) ackedKeys() [][2]string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out [][2]string

	for ref, attempts := range l.keys {
		if !slices.ContainsFunc(attempts, func(a attempt) bool { return a.acked }) {
			continue
		}

		bucket, key, ok := strings.Cut(ref, "/")
		if ok {
			out = append(out, [2]string{bucket, key})
		}
	}

	return out
}

// s3Clients builds one anonymous minio client per node.
func s3Clients(t *testing.T, nodes []*node) []*minio.Client {
	t.Helper()

	out := make([]*minio.Client, len(nodes))

	for i, n := range nodes {
		c, err := minio.New(n.s3Addr, &minio.Options{Secure: false})
		require.NoError(t, err)

		out[i] = c
	}

	return out
}

// putRetry PUTs through any node, sweeping them until one acks or the
// deadline passes.
func putRetry(ctx context.Context, clients []*minio.Client, bucket, key string, data []byte) error {
	deadline := time.Now().Add(45 * time.Second)

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
			return fmt.Errorf("put %s/%s: no node acked before the deadline", bucket, key)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// readAnyHash GETs the object through any node and returns its content MD5.
func readAnyHash(ctx context.Context, clients []*minio.Client, bucket, key string) (string, error) {
	var lastErr error

	for range 3 { // Sweep the nodes a few times: transient per-node failures are not losses.
		for _, c := range clients {
			opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)

			obj, err := c.GetObject(opCtx, bucket, key, minio.GetObjectOptions{})
			if err != nil {
				cancel()

				lastErr = err

				continue
			}

			h := md5.New()
			_, err = io.Copy(h, obj)
			_ = obj.Close()

			cancel()

			if err != nil {
				lastErr = err
				continue
			}

			return hex.EncodeToString(h.Sum(nil)), nil
		}

		time.Sleep(500 * time.Millisecond)
	}

	return "", lastErr
}

// TestChaos is the scripted fault sequence under continuous load.
func TestChaos(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos suite is long-running")
	}

	e := startEtcd(t)

	// Four nodes up.
	nodes := make([]*node, 0, 5)

	for i := range 4 {
		n := newNode(t, i, e.clientURL.String())
		n.start(t)
		nodes = append(nodes, n)
	}

	for _, n := range nodes {
		n.waitHealthy(t)
	}

	// clients is read by the writer goroutines and replaced when a node
	// joins; the atomic pointer keeps the swap race-free.
	var clients atomic.Pointer[[]*minio.Client]

	setClients := func(cs []*minio.Client) { clients.Store(&cs) }
	setClients(s3Clients(t, nodes))

	// Mixed-scheme buckets: default rf2.5, plus rf3 and ec:2,1 via the CLI.
	ctx := t.Context()

	for _, bucket := range chaosBuckets {
		require.NoError(t, (*clients.Load())[0].MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))
	}

	nodes[0].runCLI(t, "cluster", "scheme", "b-rf3", "rf3")
	nodes[0].runCLI(t, "cluster", "scheme", "b-ec", "ec:2,1")

	// Continuous load: each writer owns its keys, so the last acked payload
	// per key is deterministic.
	lg := newLedger()

	loadCtx, stopLoad := context.WithCancel(ctx)

	var writers sync.WaitGroup

	const (
		writerCount   = 4
		keysPerWriter = 15
	)

	for w := range writerCount {
		writers.Add(1)

		go func() {
			defer writers.Done()

			rnd := rand.New(rand.NewSource(int64(w)))

			for i := 0; loadCtx.Err() == nil; i++ {
				bucket := chaosBuckets[i%len(chaosBuckets)]
				key := fmt.Sprintf("w%d-k%d", w, rnd.Intn(keysPerWriter))

				data := make([]byte, 1024+rnd.Intn(7*1024))
				rnd.Read(data)

				sum := md5.Sum(data)
				idx := lg.begin(bucket, key, hex.EncodeToString(sum[:]))

				if err := putRetry(loadCtx, *clients.Load(), bucket, key, data); err == nil {
					lg.ack(bucket, key, idx)
				}

				time.Sleep(50 * time.Millisecond)
			}
		}()
	}

	step := func(name string, d time.Duration) {
		t.Logf("chaos: %s (%s under load)", name, d)
		time.Sleep(d)
	}

	step("warmup", 5*time.Second)

	// Node crash: SIGKILL, TTL-expire out of the topology, restart.
	nodes[3].kill()
	step("n3 killed", 8*time.Second)

	nodes[3].start(t)
	nodes[3].waitHealthy(t)
	step("n3 restarted", 4*time.Second)

	// Disk loss: crash the node, wipe its disk, restart empty — scrub and
	// repair rebuild it.
	nodes[2].kill()
	require.NoError(t, os.RemoveAll(nodes[2].diskRoot))
	nodes[2].start(t)
	nodes[2].waitHealthy(t)
	step("n2 disk wiped and restarted", 4*time.Second)

	// Cluster growth: a fifth node joins; auto-rebalance converges it.
	added := newNode(t, 4, e.clientURL.String())
	added.start(t)
	added.waitHealthy(t)
	nodes = append(nodes, added)
	setClients(s3Clients(t, nodes))

	step("n4 added", 6*time.Second)

	// Control-plane outage: etcd hard-restarts on the same data.
	e.restart(t)
	step("etcd restarted", 8*time.Second)

	stopLoad()
	writers.Wait()

	for _, n := range nodes {
		n.waitHealthy(t)
	}

	// Convergence: every object fully present at the current placement — the
	// "no under-protected object" invariant, checked fragment by fragment.
	// The nodes' own scrubs and auto-rebalance must do this; the verifier
	// only observes.
	coord, verifier := newVerifier(t, e.clientURL.String())

	converged := func() bool {
		plan, err := verifier.PlanRebalance(ctx)
		if err != nil {
			t.Logf("chaos: plan: %v", err)
			return false
		}

		t.Logf("chaos: convergence check: %d objects, %d misplaced, %d unplannable, %s to move",
			plan.Objects, plan.MisplacedObjects, plan.Unplannable, fmtBytes(plan.MisplacedBytes))

		return plan.MisplacedObjects == 0 && plan.Unplannable == 0
	}

	deadline := time.Now().Add(3 * time.Minute)
	for !converged() {
		if time.Now().After(deadline) {
			diagnose(ctx, t, coord, verifier, nodes, lg)
			t.Fatal("cluster did not converge to full protection")
		}

		time.Sleep(3 * time.Second)
	}

	// No acked write lost: every acknowledged PUT reads back as an
	// acknowledged-or-later payload.
	keys := lg.ackedKeys()
	require.NotEmpty(t, keys, "the load generator must have acked writes")

	t.Logf("chaos: verifying %d acked keys", len(keys))

	for _, bk := range keys {
		lg.verify(t, bk[0], bk[1], func() (string, error) {
			return readAnyHash(ctx, *clients.Load(), bk[0], bk[1])
		})
	}
}

// diagnose reports why convergence failed: a direct repair of every object
// (surfacing per-object errors and residual changes) and the tail of each
// node's log.
func diagnose(ctx context.Context, t *testing.T, coord *clusterstore.Coordinator, verifier *clusterstore.Repairer, nodes []*node, lg *ledger) {
	t.Helper()

	for _, bucket := range chaosBuckets {
		scs, err := coord.ListObjects(ctx, bucket, "")
		if err != nil {
			t.Logf("diagnose: list %s: %v", bucket, err)
			continue
		}

		for _, sc := range scs {
			rep, err := verifier.RepairObject(ctx, bucket, sc.Key)

			switch {
			case err != nil:
				t.Logf("diagnose: repair %s/%s (scheme %s, gen %s, seq %d, checksum %s): %v",
					bucket, sc.Key, sc.Scheme, sc.Generation, sc.Seq, sc.Checksum, err)
				lg.dump(t, bucket, sc.Key, sc.Checksum)
			case rep.Changed():
				t.Logf("diagnose: repair %s/%s (scheme %s) still changed state: %+v", bucket, sc.Key, sc.Scheme, *rep)
			}
		}
	}

	if plan, err := verifier.PlanRebalance(ctx); err == nil {
		t.Logf("diagnose: post-repair plan: %d misplaced, %d unplannable", plan.MisplacedObjects, plan.Unplannable)
	}

	for _, n := range nodes {
		if data, err := os.ReadFile(n.logPath); err == nil {
			tail := data
			if len(tail) > 4096 {
				tail = tail[len(tail)-4096:]
			}

			t.Logf("diagnose: %s log tail:\n%s", n.id, tail)
		}
	}
}

// newVerifier builds a disk-less clusterstore client for the invariant
// checks, the same shape as `fs cluster rebalance --dry-run`.
func newVerifier(t *testing.T, etcdURL string) (*clusterstore.Coordinator, *clusterstore.Repairer) {
	t.Helper()

	client, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdURL}, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	source, err := etcd.NewSource(t.Context(), client, etcd.Config{Prefix: etcdPrefix})
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })

	coord, err := clusterstore.New(clusterstore.Config{
		Topology: source,
		Peers:    clusterstore.NewHTTPPeers(cluster.NodeID("chaos/verifier"), nil, transport.Secret(clusterSecret), nil),
		Scheme:   func(string) scheme.Scheme { return scheme.Default },
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = coord.Close() })

	r, err := clusterstore.NewRepairer(clusterstore.RepairerConfig{Coordinator: coord, Self: "chaos/verifier"})
	require.NoError(t, err)

	return coord, r
}

// fmtBytes renders a byte count for progress logs.
func fmtBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}

	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}
