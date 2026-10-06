// Command chaos runs a soak test of a cluster: real fs processes under a
// mixed workload while nodes are killed, frozen and moved in and out of the
// layout, checking after every round that the cluster lost no acknowledged
// write, served no torn object and brought nothing deleted back.
//
//	go run ./scripts/chaos -fs ./fs -rounds 8
//
// Each worker owns its keys and runs one operation at a time on them, so the
// expected state of every key is known: the value of its last acknowledged
// write, plus any value a failed write may still deliver — a write whose
// response was lost has an unknown outcome and may land later. A read that
// returns anything else is a violation.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/go-faster/fs/adminapi"
)

const (
	accessKey = "CHAOSACCESSKEY"
	secretKey = "chaos-secret-key-0123456789abcdef"
	token     = "chaos-admin-token"
	secret    = "chaos-cluster-secret-0123456789"
)

// Buckets the workload writes, one per storage path.
var buckets = []struct{ name, scheme string }{
	{"replicated", ""}, {"versioned", ""}, {"coded", "ec:4,2"},
}

// node is one fs process.
type node struct {
	id, zone        string
	s3, peer, admin int
	dir             string
	member          bool
	mu              sync.Mutex
	cmd             *exec.Cmd
	up              bool
	client          *minio.Client
}

type cluster struct {
	bin   string
	nodes []*node
}

func (c *cluster) config(n *node) string {
	return fmt.Sprintf(`server:
  addr: "127.0.0.1:%d"
storage:
  root: data
  background:
    sync_interval: 3s
    resync_interval: 1s
    gc_interval: 10s
    gc_grace: 60s
    tombstone_delay: 20s
auth:
  keys:
    - access_key: %s
      secret_key: %s
      grants:
        - bucket: "*"
          permission: admin
admin: {enabled: true, addr: "127.0.0.1:%d", token: %s}
lifecycle: {interval: 0s}
cluster:
  node_id: %s
  addr: "127.0.0.1:%d"
  advertise_addr: "127.0.0.1:%d"
  peers: ["127.0.0.1:%d"]
  secret: %s
`, n.s3, accessKey, secretKey, n.admin, token, n.id, n.peer, n.peer, c.nodes[0].peer, secret)
}

func (c *cluster) start(n *node) error {
	if err := os.WriteFile(filepath.Join(n.dir, "server.yaml"), []byte(c.config(n)), 0o600); err != nil {
		return err
	}

	log, err := os.OpenFile(filepath.Join(n.dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}

	cmd := exec.Command(c.bin, "s3", "--config", "server.yaml") //nolint:gosec // The harness runs the binary it was given.
	cmd.Dir, cmd.Stdout, cmd.Stderr = n.dir, log, log

	cmd.Env = append(os.Environ(), "OTEL_TRACES_EXPORTER=none", "OTEL_METRICS_EXPORTER=none", "OTEL_LOGS_EXPORTER=none")

	if err := cmd.Start(); err != nil {
		return err
	}

	n.mu.Lock()
	n.cmd, n.up = cmd, true
	n.mu.Unlock()

	for range 120 {
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", n.s3)); err == nil { //nolint:noctx // Harness.
			_ = resp.Body.Close()

			return nil
		}

		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("%s did not become healthy; see %s/server.log", n.id, n.dir)
}

// kill stops n with SIGKILL: no shutdown, as a crash or a power loss.
func (c *cluster) kill(n *node) {
	n.mu.Lock()
	cmd := n.cmd
	n.up = false
	n.mu.Unlock()

	if cmd != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// stop shuts n down gracefully.
func (c *cluster) stop(n *node) {
	n.mu.Lock()
	cmd := n.cmd
	n.up = false
	n.mu.Unlock()

	if cmd != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}
}

func (c *cluster) signal(n *node, sig syscall.Signal, up bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.up = up
	_ = n.cmd.Process.Signal(sig)
}

func (c *cluster) live() []*node {
	var out []*node

	for _, n := range c.nodes {
		n.mu.Lock()
		if n.up {
			out = append(out, n)
		}
		n.mu.Unlock()
	}

	return out
}

func (c *cluster) admin(n *node) *adminapi.Client {
	client, err := adminapi.NewClient(fmt.Sprintf("http://127.0.0.1:%d", n.admin), adminapi.WithClient(&http.Client{
		Timeout:   30 * time.Second,
		Transport: bearer{},
	}))
	if err != nil {
		panic(err)
	}

	return client
}

type bearer struct{}

func (bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+token)

	return http.DefaultTransport.RoundTrip(r)
}

// applyLayout makes the members the layout and waits until every live node
// has adopted it.
func (c *cluster) applyLayout(ctx context.Context) error {
	req := &adminapi.ApplyLayoutRequest{Widths: []int{3, 6}, Partitions: adminapi.NewOptInt(64)}

	for _, n := range c.nodes {
		if n.member {
			req.Members = append(req.Members, adminapi.LayoutRole{ID: n.id, Zone: adminapi.NewOptString(n.zone), Capacity: 1 << 40})
		}
	}

	change, err := c.admin(c.live()[0]).ApplyLayout(ctx, req, adminapi.ApplyLayoutParams{})
	if err != nil {
		return fmt.Errorf("apply layout: %w", err)
	}

	fmt.Printf("  %s layout %d applied\n", time.Now().Format("15:04:05.000"), change.Layout.Version)

	for _, n := range c.live() {
		for attempt := 0; ; attempt++ {
			l, err := c.admin(n).GetLayout(ctx)
			if err == nil && l.Version >= change.Layout.Version {
				break
			}

			if attempt == 240 {
				return fmt.Errorf("%s never adopted layout %d", n.id, change.Layout.Version)
			}

			time.Sleep(250 * time.Millisecond)
		}
	}

	return nil
}

// keyModel is what a key may hold: hashes of content, "" for absent.
type keyModel struct {
	bucket, key string
	seq         int
	possible    map[string]bool
	// failed are writes since the last acknowledged one whose outcome is
	// unknown: one may still land, even after a later delete.
	failed map[string]bool
	// history is the key's recent operations, for a violation report.
	history []string
}

func (k *keyModel) note(format string, args ...any) {
	k.history = append(k.history, time.Now().Format("15:04:05.000")+" "+fmt.Sprintf(format, args...))
	if len(k.history) > 12 {
		k.history = k.history[1:]
	}
}

func (k *keyModel) set(vals ...string) {
	k.possible = map[string]bool{}
	for _, v := range vals {
		k.possible[v] = true
	}
}

type violation struct {
	round         int
	phase, bucket string
	key, observed string
	possible      []string
	history       []string
}

type harness struct {
	c          *cluster
	rng        *rand.Rand
	keys       [][]*keyModel // by worker
	violations []violation
	vmu        sync.Mutex

	ok, failed, unavailable atomic.Int64
	round                   int
}

func content(key string, seq, size int) []byte {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", key, seq))
	r := rand.New(rand.NewChaCha8(sum)) //nolint:gosec // Reproducible content, not secrets.
	b := make([]byte, size)

	for i := 0; i+8 <= len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], r.Uint64())
	}

	return b
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)

	return hex.EncodeToString(s[:8])
}

func (h *harness) violate(phase string, k *keyModel, observed string) {
	h.vmu.Lock()
	defer h.vmu.Unlock()

	var possible []string
	for v := range k.possible {
		possible = append(possible, v)
	}

	slices.Sort(possible)
	h.violations = append(h.violations, violation{h.round, phase, k.bucket, k.key, observed, possible, slices.Clone(k.history)})
}

// read returns the hash of what the key holds now, "" for absent.
func read(ctx context.Context, n *node, k *keyModel) (string, error) {
	obj, err := n.client.GetObject(ctx, k.bucket, k.key, minio.GetObjectOptions{})
	if err != nil {
		return "", err
	}

	defer func() { _ = obj.Close() }()

	b, err := io.ReadAll(obj)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return "", nil
		}

		return "", err
	}

	return hashOf(b), nil
}

func size(r *rand.Rand) int {
	switch p := r.IntN(100); {
	case p < 30:
		return 200 // Inline.
	case p < 60:
		return 40 << 10 // One block, under the coded minimum.
	case p < 85:
		return 400 << 10 // One coded block.
	case p < 95:
		return 2500 << 10 // Three blocks.
	default:
		return 11 << 20 // Multipart.
	}
}

func (h *harness) worker(ctx context.Context, w int, seed uint64) {
	r := rand.New(rand.NewPCG(seed, uint64(w))) //nolint:gosec // A seeded, reproducible workload.

	for ctx.Err() == nil {
		live := h.c.live()
		if len(live) == 0 {
			time.Sleep(100 * time.Millisecond)

			continue
		}

		n := live[r.IntN(len(live))]
		k := h.keys[w][r.IntN(len(h.keys[w]))]

		opCtx, cancel := context.WithTimeout(ctx, 20*time.Second)

		switch p := r.IntN(100); {
		case p < 50:
			k.seq++
			body := content(k.key, k.seq, size(r))
			v := hashOf(body)

			_, err := n.client.PutObject(opCtx, k.bucket, k.key, bytes.NewReader(body), int64(len(body)),
				minio.PutObjectOptions{PartSize: 5 << 20})
			if err != nil {
				h.failed.Add(1)

				k.possible[v], k.failed[v] = true, true
				k.note("put %s via %s FAILED: %v", v, n.id, err)
			} else {
				h.ok.Add(1)
				k.set(v)
				k.failed = map[string]bool{}
				k.note("put %s via %s ok", v, n.id)
			}
		case p < 70:
			err := n.client.RemoveObject(opCtx, k.bucket, k.key, minio.RemoveObjectOptions{})
			if err != nil {
				h.failed.Add(1)

				k.possible[""] = true
				k.note("delete via %s FAILED: %v", n.id, err)
			} else {
				k.note("delete via %s ok", n.id)
				h.ok.Add(1)

				// A delete removes the version it saw. On an unversioned
				// bucket a failed write may still land after it, as a newer
				// version; on a versioned one the marker is newer than it.
				k.set("")

				if k.bucket != "versioned" {
					for v := range k.failed {
						k.possible[v] = true
					}
				}
			}
		default:
			got, err := read(opCtx, n, k)
			switch {
			case err != nil:
				h.unavailable.Add(1)
			case !k.possible[got]:
				k.note("get via %s read %q", n.id, got)
				h.violate("load", k, got)
			default:
				h.ok.Add(1)
				k.note("get via %s read %q", n.id, got)
			}
		}

		cancel()
	}
}

// verify reads every key once the cluster is healthy: any error is a
// failure now, not unavailability.
func (h *harness) verify(ctx context.Context) int {
	errs := 0

	for _, ks := range h.keys {
		for _, k := range ks {
			var (
				got string
				err error
			)

			for range 3 {
				live := h.c.live()

				got, err = read(ctx, live[h.rng.IntN(len(live))], k)
				if err == nil {
					break
				}

				time.Sleep(time.Second)
			}

			switch {
			case err != nil:
				errs++

				fmt.Printf("  read %s/%s: %v\n", k.bucket, k.key, err)
			case !k.possible[got]:
				h.violate("verify", k, got)
			}
		}
	}

	return errs
}

func main() {
	bin := flag.String("fs", "./fs", "path to the fs binary")
	dir := flag.String("dir", "", "working directory (default: a temporary one, removed on success)")
	rounds := flag.Int("rounds", 8, "chaos rounds")
	workers := flag.Int("workers", 6, "concurrent workers")
	keysPer := flag.Int("keys", 12, "keys per worker")
	load := flag.Duration("load", 20*time.Second, "load before and after each disruption")
	seed := flag.Uint64("seed", uint64(time.Now().UnixNano()), "random seed")
	only := flag.String("actions", "", "comma-separated action numbers to cycle through (default all: 1 kill one, 2 freeze, 3 kill two, 4 add node, 5 remove node)")

	flag.Parse()

	if err := run(*bin, *dir, *rounds, *workers, *keysPer, *load, *seed, *only); err != nil {
		fmt.Fprintln(os.Stderr, "chaos:", err)
		os.Exit(1)
	}
}

func run(bin, dir string, rounds, workers, keysPer int, load time.Duration, seed uint64, only string) error {
	abs, err := filepath.Abs(bin)
	if err != nil {
		return err
	}

	keep := dir != ""
	if dir == "" {
		if dir, err = os.MkdirTemp("", "fs-chaos-*"); err != nil {
			return err
		}
	}

	fmt.Printf("seed %d, data in %s\n", seed, dir)

	minio.MaxRetry = 1

	c := &cluster{bin: abs}
	zones := []string{"a", "b", "c", "a", "b", "c", "a"}

	for i := range 7 {
		n := &node{
			id: fmt.Sprintf("n%d", i+1), zone: zones[i], member: i < 6,
			s3: 18400 + i, peer: 18500 + i, admin: 18600 + i,
			dir: filepath.Join(dir, fmt.Sprintf("n%d", i+1)),
		}

		if err := os.MkdirAll(n.dir, 0o750); err != nil {
			return err
		}

		n.client, err = minio.New(fmt.Sprintf("127.0.0.1:%d", n.s3), &minio.Options{
			Creds: credentials.NewStaticV4(accessKey, secretKey, ""),
			Transport: &http.Transport{
				ResponseHeaderTimeout: 15 * time.Second,
				MaxIdleConnsPerHost:   16,
			},
		})
		if err != nil {
			return err
		}

		c.nodes = append(c.nodes, n)
	}

	defer func() {
		for _, n := range c.nodes {
			n.mu.Lock()
			cmd := n.cmd
			n.mu.Unlock()

			if cmd != nil && cmd.ProcessState == nil {
				_ = cmd.Process.Signal(syscall.SIGCONT)
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		}
	}()

	for _, n := range c.nodes[:6] {
		if err := c.start(n); err != nil {
			return err
		}
	}

	ctx := context.Background()

	if err := c.applyLayout(ctx); err != nil {
		return err
	}

	n1 := c.nodes[0]

	for _, b := range buckets {
		if err := n1.client.MakeBucket(ctx, b.name, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("make bucket %s: %w", b.name, err)
		}

		if b.scheme != "" {
			if _, err := c.admin(n1).SetBucketScheme(ctx, &adminapi.BucketScheme{Scheme: b.scheme},
				adminapi.SetBucketSchemeParams{Bucket: b.name}); err != nil {
				return fmt.Errorf("bucket scheme: %w", err)
			}
		}
	}

	if err := n1.client.EnableVersioning(ctx, "versioned"); err != nil {
		return err
	}

	h := &harness{c: c, rng: rand.New(rand.NewPCG(seed, 1))} //nolint:gosec // A seeded, reproducible workload.

	for w := range workers {
		var ks []*keyModel

		for i := range keysPer {
			b := buckets[(w+i)%len(buckets)].name
			ks = append(ks, &keyModel{
				bucket: b, key: fmt.Sprintf("w%d/k%d", w, i),
				possible: map[string]bool{"": true}, failed: map[string]bool{},
			})
		}

		h.keys = append(h.keys, ks)
	}

	actions := []struct {
		name string
		do   func() error
	}{
		{"kill one node", func() error {
			n := h.pick(1)[0]
			fmt.Printf("  SIGKILL %s\n", n.id)
			c.kill(n)
			time.Sleep(load)

			return c.start(n)
		}},
		{"freeze one node past the tombstone delay", func() error {
			n := h.pick(1)[0]
			fmt.Printf("  SIGSTOP %s for 45s\n", n.id)
			c.signal(n, syscall.SIGSTOP, false)
			time.Sleep(45 * time.Second)
			c.signal(n, syscall.SIGCONT, true)

			return nil
		}},
		{"kill two nodes in different zones", func() error {
			ns := h.pick(2)
			fmt.Printf("  SIGKILL %s, %s\n", ns[0].id, ns[1].id)

			for _, n := range ns {
				c.kill(n)
			}

			time.Sleep(load)

			for _, n := range ns {
				if err := c.start(n); err != nil {
					return err
				}
			}

			return nil
		}},
		{"add a node to the layout", func() error {
			spare := c.nodes[6]
			if spare.member {
				return nil
			}

			if err := c.start(spare); err != nil {
				return err
			}

			spare.member = true
			fmt.Printf("  %s joins\n", spare.id)

			return c.applyLayout(ctx)
		}},
		{"remove it again", func() error {
			spare := c.nodes[6]
			if !spare.member {
				return nil
			}

			spare.member = false
			fmt.Printf("  %s leaves; handing over\n", spare.id)

			if err := c.applyLayout(ctx); err != nil {
				return err
			}

			// Handover runs on the sweeps a layout change starts.
			time.Sleep(20 * time.Second)
			c.stop(spare)

			return nil
		}},
	}

	if only != "" {
		var picked []int

		for _, f := range strings.Split(only, ",") {
			var i int
			if _, err := fmt.Sscan(f, &i); err != nil || i < 1 || i > len(actions) {
				return fmt.Errorf("bad action %q", f)
			}

			picked = append(picked, i-1)
		}

		var sel []struct {
			name string
			do   func() error
		}

		for _, i := range picked {
			sel = append(sel, actions[i])
		}

		actions = sel
	}

	readErrs := 0

	for round := range rounds {
		h.round = round + 1
		act := actions[round%len(actions)]
		fmt.Printf("round %d: %s\n", h.round, act.name)

		loadCtx, stop := context.WithCancel(ctx)

		var wg sync.WaitGroup

		for w := range workers {
			wg.Go(func() { h.worker(loadCtx, w, seed+uint64(round)) }) //nolint:gosec // round is small and positive.
		}

		time.Sleep(load)

		actErr := act.do()

		time.Sleep(load)
		stop()
		wg.Wait()

		if actErr != nil {
			return fmt.Errorf("round %d: %w", h.round, actErr)
		}

		// Settle: every member up, then a few sweeps for repair.
		time.Sleep(15 * time.Second)

		errs := h.verify(ctx)
		readErrs += errs

		fmt.Printf("  ops ok %d, failed %d, unavailable reads %d; verify read errors %d; violations so far %d\n",
			h.ok.Load(), h.failed.Load(), h.unavailable.Load(), errs, len(h.violations))
	}

	for _, v := range h.violations {
		fmt.Printf("VIOLATION round %d (%s): %s/%s read %q, expected one of %q\n",
			v.round, v.phase, v.bucket, v.key, v.observed, v.possible)

		for _, line := range v.history {
			fmt.Println("    " + line)
		}
	}

	if len(h.violations) > 0 || readErrs > 0 {
		return fmt.Errorf("%d violations, %d read errors with every node up; data kept in %s",
			len(h.violations), readErrs, dir)
	}

	fmt.Println("PASS")

	if !keep {
		_ = os.RemoveAll(dir)
	}

	return nil
}

// pick returns n distinct member nodes that are up, in distinct zones.
func (h *harness) pick(n int) []*node {
	var out []*node

	zones := map[string]bool{}

	for _, i := range h.rng.Perm(len(h.c.nodes)) {
		nd := h.c.nodes[i]
		if !nd.member || zones[nd.zone] || !slices.Contains(h.c.live(), nd) {
			continue
		}

		zones[nd.zone] = true
		out = append(out, nd)

		if len(out) == n {
			break
		}
	}

	return out
}
