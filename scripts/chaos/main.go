//go:build unix

// Command chaos runs a soak test of a cluster: real fs processes under a
// mixed workload while nodes are killed, frozen and moved in and out of the
// layout, checking after every round that the cluster lost no acknowledged
// write, served no torn object and brought nothing deleted back, and that
// bucket usage agrees with what the buckets list. The versioned bucket has
// lifecycle rules a moved sweep clock makes due at once, so noncurrent
// versions and delete markers are removed while the disruptions run; at the
// end every key must hold no more than its rule keeps.
//
//	go run ./scripts/chaos -fs ./fs -rounds 8
//
// Each worker owns its keys and runs one operation at a time on them, so the
// expected state of every key is known: the value of its last acknowledged
// write, plus any value a failed write may still deliver — a write whose
// response was lost has an unknown outcome and may land later, and one held
// by a frozen node lands when it resumes, after the writes acknowledged
// meanwhile. A read that returns anything else is a violation.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"

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
	// frozen is set while the node is stopped, and thawed is when it last
	// resumed: a request it held through the freeze completes after it.
	frozen bool
	thawed time.Time
	client *minio.Client
	// host is where the node's S3 port is, for a target on another machine;
	// empty means this one.
	host string
}

// s3Addr is the node's S3 address.
func (n *node) s3Addr() string {
	if n.host != "" {
		return net.JoinHostPort(n.host, strconv.Itoa(n.s3))
	}

	return fmt.Sprintf("127.0.0.1:%d", n.s3)
}

// thawGrace is how long after a freeze the requests a node held may still
// be completing.
const thawGrace = 5 * time.Second

// holding reports whether the node may still complete a request whose client
// gave up on it: it is frozen, or resumed only just now.
func (n *node) holding() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.frozen || time.Since(n.thawed) < thawGrace
}

type cluster struct {
	bin   string
	nodes []*node
	// secret authenticates the cluster's peers, and widths are what its
	// layout spreads for.
	secret string
	widths []int
	// bind is the host its S3 ports listen on, and tls the certificate they
	// serve, for a target served to another machine (-serve-target).
	bind string
	tls  *tlsFiles
}

// tlsFiles is a certificate and its key.
type tlsFiles struct{ cert, key string }

// realistic runs the nodes with fs's own defaults: fsync on, and the
// background work at its production cadence. The shortened intervals make a
// round exercise collection and repair within a minute; they also hide a path
// that only waits at production cadence — a retry that waits a sync interval,
// say — which is what this mode is for.
var realistic bool

// storageConfig is the node's storage section.
func storageConfig() string {
	if realistic {
		return `storage:
  root: data`
	}

	return `storage:
  root: data
  # SIGKILL does not drop the page cache, so fsync changes nothing the soak
  # tests; it only makes seven nodes on one disk wait on each other's syncs.
  fsync: none
  background:
    sync_interval: 3s
    resync_interval: 1s
    gc_interval: 10s
    gc_grace: 60s
    gc_walk_interval: 30s
    tombstone_delay: 20s`
}

// lifecycleInterval is how often each node sweeps. It is short in both modes:
// at the production cadence of 12h no pass would run within a soak.
func lifecycleInterval() string {
	if realistic {
		return "1m"
	}

	return "10s"
}

func (c *cluster) config(n *node) string {
	bind := "127.0.0.1"
	if c.bind != "" {
		bind = c.bind
	}

	server := fmt.Sprintf("server:\n  addr: %q\n", net.JoinHostPort(bind, strconv.Itoa(n.s3)))
	if c.tls != nil {
		server += fmt.Sprintf("  tls: {cert_file: %q, key_file: %q}\n", c.tls.cert, c.tls.key)
	}

	return server + fmt.Sprintf(storageConfig()+`
auth:
  keys:
    - access_key: %s
      secret_key: %s
      grants:
        - bucket: "*"
          permission: admin
admin: {enabled: true, addr: "127.0.0.1:%d", token: %s}
lifecycle: {interval: %s, clock_offset: 48h}
cluster:
  node_id: %s
  addr: "127.0.0.1:%d"
  advertise_addr: "127.0.0.1:%d"
  peers: ["127.0.0.1:%d"]
  secret: %s
`, accessKey, secretKey, n.admin, token, lifecycleInterval(), n.id, n.peer, n.peer, c.nodes[0].peer, c.secret)
}

// newNode is node id with its ports, its directory under dir and an S3
// client of it.
func newNode(id, zone string, s3, peer, admin int, dir string) (*node, error) {
	n := &node{id: id, zone: zone, s3: s3, peer: peer, admin: admin, dir: filepath.Join(dir, id)}

	if err := os.MkdirAll(n.dir, 0o750); err != nil {
		return nil, err
	}

	var err error

	n.client, err = minio.New(fmt.Sprintf("127.0.0.1:%d", n.s3), &minio.Options{
		Creds: credentials.NewStaticV4(accessKey, secretKey, ""),
		Transport: &http.Transport{
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConnsPerHost:   16,
		},
	})

	return n, err
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

	// A remote target's certificate is the only one the runner trusts.
	if targetCA != "" {
		cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+targetCA)
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	n.mu.Lock()
	n.cmd, n.up = cmd, true
	n.mu.Unlock()

	health, get := fmt.Sprintf("http://127.0.0.1:%d/health", n.s3), http.Get //nolint:noctx // Harness.
	if c.tls != nil {
		health = fmt.Sprintf("https://%s/health", net.JoinHostPort(c.bind, strconv.Itoa(n.s3)))
		get = (&http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Its own certificate, just made.
		}}).Get
	}

	for range 120 {
		if resp, err := get(health); err == nil {
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

	switch sig {
	case syscall.SIGSTOP:
		n.frozen = true
	case syscall.SIGCONT:
		n.frozen, n.thawed = false, time.Now()
	}

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
	req := &adminapi.ApplyLayoutRequest{Widths: c.widths, Partitions: adminapi.NewOptInt(64)}

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

// settleWithin bounds how long a layout change may take to complete.
var settleWithin = 2 * time.Minute

// settle waits until the layout change has completed — every node has
// synced it and no older version is in use — which is when a node that left
// may be shut down.
func (c *cluster) settle(ctx context.Context, within time.Duration) error {
	start := time.Now()
	deadline := start.Add(within)

	for time.Now().Before(deadline) {
		done := true

		for _, n := range c.live() {
			l, err := c.admin(n).GetLayout(ctx)
			if err != nil || len(l.RetainedVersions) > 0 {
				done = false

				break
			}
		}

		if done {
			fmt.Printf("  %s layout change complete in %s\n", time.Now().Format("15:04:05.000"), time.Since(start).Round(time.Second))

			return nil
		}

		time.Sleep(time.Second)
	}

	// Say who is holding it up.
	for _, n := range c.live() {
		l, err := c.admin(n).GetLayout(ctx)
		if err != nil {
			fmt.Printf("  %s: layout: %v\n", n.id, err)

			continue
		}

		list, err := c.admin(n).ListClusterNodes(ctx)
		if err != nil {
			fmt.Printf("  %s: nodes: %v\n", n.id, err)

			continue
		}

		var synced []string
		for _, p := range list.Nodes {
			synced = append(synced, fmt.Sprintf("%s=%d", p.ID.Or("?"), p.SyncedVersion.Or(0)))
		}

		fmt.Printf("  %s: version %d, retained %v, synced %v\n", n.id, l.Version, l.RetainedVersions, synced)

		// A node behind gets a goroutine dump in its log: SIGQUIT.
		for _, p := range list.Nodes {
			if p.Self && p.SyncedVersion.Or(0) < l.Version {
				fmt.Printf("  %s is behind; goroutines dumped to %s/server.log\n", n.id, n.dir)

				n.mu.Lock()
				_ = n.cmd.Process.Signal(syscall.SIGQUIT)
				n.mu.Unlock()
			}
		}
	}

	time.Sleep(2 * time.Second)

	return fmt.Errorf("layout change did not complete within %s", within)
}

// keyModel is what a key may hold: hashes of content, "" for absent.
type keyModel struct {
	bucket, key string
	seq         int
	possible    map[string]bool
	// failed are writes since the last acknowledged one whose outcome is
	// unknown, by the node they were sent to: one may still land, even after
	// a later delete.
	failed map[string]*node
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

// held keeps the failed writes a frozen node may still complete, and makes
// them possible: the node commits one when it resumes, as the key's newest
// version, after any write acknowledged meanwhile.
func (k *keyModel) held() {
	for v, n := range k.failed {
		if n.holding() {
			k.possible[v] = true
		} else {
			delete(k.failed, v)
		}
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

				k.possible[v], k.failed[v] = true, n
				k.note("put %s via %s FAILED: %v", v, n.id, err)
			} else {
				h.ok.Add(1)
				k.set(v)
				k.held()
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
				// version; on a versioned one the marker is newer than it,
				// unless a frozen node still holds the write.
				k.set("")

				if k.bucket != "versioned" {
					for v := range k.failed {
						k.possible[v] = true
					}
				} else {
					k.held()
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
	settle := flag.Duration("settle", 2*time.Minute, "how long a layout change may take to complete")
	only := flag.String("actions", "", "comma-separated action numbers to cycle through (default all: 1 kill one, 2 freeze, 3 kill two, 4 add node, 5 remove node)")

	realisticFlag := flag.Bool("realistic", false, "run the nodes with fs's defaults (fsync on, production background cadence) instead of shortened intervals")

	flag.BoolVar(&replicate, "replication", true, "replicate the versioned bucket to a second, three-node deployment through a fault-injecting proxy")
	flag.StringVar(&serveTarget, "serve-target", "", "run only the replication target, its S3 ports on this host over TLS, for a soak on another machine (needs -dir)")
	flag.StringVar(&targetHost, "target", "", "replicate to the target that -serve-target runs on this host, instead of a local one")
	flag.StringVar(&targetCA, "target-ca", "", "the remote target's certificate ("+targetCert+" in its -dir)")

	flag.Parse()

	settleWithin = *settle
	realistic = *realisticFlag

	// At production cadence nothing is collected for an hour, so every round
	// adds data, and a node joining late pulls tens of gigabytes through one
	// shared disk. Two minutes measures the disk, not fs; each change still
	// prints how long it took.
	settleSet := false

	flag.Visit(func(f *flag.Flag) { settleSet = settleSet || f.Name == "settle" })

	if realistic && !settleSet {
		settleWithin = 10 * time.Minute
	}

	if serveTarget != "" {
		if err := serveTargetOnly(*bin, *dir, serveTarget); err != nil {
			fmt.Fprintln(os.Stderr, "chaos:", err)
			os.Exit(1)
		}

		return
	}

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

	c := &cluster{bin: abs, secret: secret, widths: []int{3, 6}}
	zones := []string{"a", "b", "c", "a", "b", "c", "a"}

	// A port someone else holds — a node left over from an earlier soak, say —
	// would make a node of this one fail to start while the harness talks to
	// a stranger in its place, and every finding after that is noise.
	ports := []int{proxyPort}

	for i := range 7 {
		ports = append(ports, 18400+i, 18500+i, 18600+i)
	}

	if targetHost == "" {
		for i := range 3 {
			ports = append(ports, 18700+i, 18800+i, 18900+i)
		}
	}

	for _, port := range ports {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return fmt.Errorf("port %d is taken; stop whatever holds it (a node of an earlier soak?): %w", port, err)
		}

		_ = l.Close()
	}

	for i := range 7 {
		n, err := newNode(fmt.Sprintf("n%d", i+1), zones[i], 18400+i, 18500+i, 18600+i, dir)
		if err != nil {
			return err
		}

		n.member = i < 6
		c.nodes = append(c.nodes, n)
	}

	// The replication target: another deployment of three nodes, reached
	// through a proxy that injects network faults.
	var dst *cluster

	switch {
	case replicate && targetHost != "":
		if dst, err = remoteTarget(targetHost); err != nil {
			return err
		}
	case replicate:
		if dst, err = newTarget(abs, dir); err != nil {
			return err
		}
	}

	// Every node is killed before the data goes: a node still running would
	// write into the directory as it is being removed.
	killAll := func() {
		all := c.nodes
		if dst != nil {
			all = append(slices.Clone(all), dst.nodes...)
		}

		for _, n := range all {
			n.mu.Lock()
			cmd := n.cmd
			n.mu.Unlock()

			if cmd != nil && cmd.ProcessState == nil {
				_ = cmd.Process.Signal(syscall.SIGCONT)
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		}
	}

	defer killAll()

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

	if err := n1.client.SetBucketLifecycle(ctx, "versioned", lifecycleRules(workers)); err != nil {
		return fmt.Errorf("lifecycle: %w", err)
	}

	// A limit the soak never reaches, so that every write goes through
	// enforcement and its usage cache without being refused.
	if _, err := c.admin(n1).SetBucketQuota(ctx, &adminapi.Quota{
		Enabled: true, MaxObjects: adminapi.NewOptInt64(1 << 40), MaxSize: adminapi.NewOptInt64(1 << 60),
	}, adminapi.SetBucketQuotaParams{Bucket: "replicated"}); err != nil {
		return fmt.Errorf("quota: %w", err)
	}

	var repl *replication

	if dst != nil {
		if repl, err = setupReplication(ctx, c, dst, seed); err != nil {
			return err
		}
	}

	h := &harness{c: c, rng: rand.New(rand.NewPCG(seed, 1))} //nolint:gosec // A seeded, reproducible workload.

	for w := range workers {
		var ks []*keyModel

		for i := range keysPer {
			b := buckets[(w+i)%len(buckets)].name
			ks = append(ks, &keyModel{
				bucket: b, key: fmt.Sprintf("w%d/k%d", w, i),
				possible: map[string]bool{"": true}, failed: map[string]*node{},
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
		{freezeName(), func() error {
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

			if err := c.applyLayout(ctx); err != nil {
				return err
			}

			return c.settle(ctx, settleWithin)
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

			// A node that leaves may go once every node has synced the change:
			// until then reads may still come to it.
			if err := c.settle(ctx, settleWithin); err != nil {
				return err
			}

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

	var keyErrs, usageErrs []string

	for round := range rounds {
		h.round = round + 1
		act := actions[round%len(actions)]
		fmt.Printf("round %d: %s\n", h.round, act.name)

		loadCtx, stop := context.WithCancel(ctx)

		var wg sync.WaitGroup

		for w := range workers {
			wg.Go(func() { h.worker(loadCtx, w, seed+uint64(round)) }) //nolint:gosec // round is small and positive.
		}

		if repl != nil {
			wg.Go(func() { repl.disrupt(loadCtx, round, load) })
			wg.Go(func() { repl.deleteAtTarget(loadCtx, seed+uint64(round)) }) //nolint:gosec // round is small and positive.
			wg.Go(func() { repl.watch(loadCtx) })
		}

		time.Sleep(load)

		actErr := act.do()

		time.Sleep(load)
		stop()
		wg.Wait()

		if actErr != nil {
			h.report()

			return fmt.Errorf("round %d: %w", h.round, actErr)
		}

		// Settle: every member up, then a few sweeps for repair.
		time.Sleep(15 * time.Second)

		errs := h.verify(ctx)
		readErrs += errs

		if err := h.checkKeys(ctx); err != nil {
			keyErrs = append(keyErrs, fmt.Sprintf("round %d: %v", h.round, err))
			fmt.Println("  KEYS:", err)
		}

		if err := h.checkUsage(ctx); err != nil {
			usageErrs = append(usageErrs, fmt.Sprintf("round %d: %v", h.round, err))
			fmt.Println("  USAGE:", err)
		}

		fmt.Printf("  ops ok %d, failed %d, unavailable reads %d; verify read errors %d; violations so far %d\n",
			h.ok.Load(), h.failed.Load(), h.unavailable.Load(), errs, len(h.violations))
	}

	lifecycleErr := h.checkLifecycle(ctx, workers)
	if lifecycleErr != nil {
		fmt.Println("LIFECYCLE", lifecycleErr)
	}

	var replErr error

	if repl != nil {
		if replErr = repl.check(ctx); replErr != nil {
			fmt.Println("REPLICATION", replErr)
		}
	}

	h.report()

	fmt.Printf("data: %.1f GiB in %s\n", float64(dirSize(dir))/(1<<30), dir)

	for _, e := range keyErrs {
		fmt.Println("KEYS " + e)
	}

	for _, e := range usageErrs {
		fmt.Println("USAGE " + e)
	}

	if len(h.violations) > 0 || readErrs > 0 || len(keyErrs) > 0 || len(usageErrs) > 0 || lifecycleErr != nil || replErr != nil {
		return fmt.Errorf("%d violations, %d read errors with every node up, %d access-key failures, %d usage mismatches, lifecycle: %v, replication: %v; data kept in %s",
			len(h.violations), readErrs, len(keyErrs), len(usageErrs), lifecycleErr, replErr, dir)
	}

	fmt.Println("PASS")

	if !keep {
		killAll()

		_ = os.RemoveAll(dir)
	}

	return nil
}

// dirSize is the bytes of the files under dir.
func dirSize(dir string) int64 {
	var n int64

	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}

		return nil
	})

	return n
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

// freezeName names the freeze action for what it does in this mode: past the
// tombstone delay only when that is shortened.
func freezeName() string {
	if realistic {
		return "freeze one node for 45s"
	}

	return "freeze one node past the tombstone delay"
}

// keyDeadline bounds how long a node may take to accept a key created through
// another, or to refuse one deleted through another: nodes refresh every 5s.
const keyDeadline = 30 * time.Second

// checkKeys creates an access key through one live node and requires every
// live node to accept it, then deletes it through another and requires every
// live node to refuse it, each within keyDeadline.
func (h *harness) checkKeys(ctx context.Context) error {
	live := h.c.live()
	if len(live) < 2 {
		return nil
	}

	id := fmt.Sprintf("CHAOSKEY%04d", h.round)
	secret := "chaos-round-key-secret-" + id

	created, deleted := live[h.rng.IntN(len(live))], live[h.rng.IntN(len(live))]

	_, err := h.c.admin(created).CreateAccessKey(ctx, &adminapi.CreateAccessKeyRequest{
		AccessKey: adminapi.NewOptString(id),
		SecretKey: adminapi.NewOptString(secret),
		Grants:    []adminapi.Grant{{Bucket: "*", Permission: adminapi.PermissionRead}},
	})
	if err != nil {
		return fmt.Errorf("create %s through %s: %w", id, created.id, err)
	}

	if err := h.everyNode(ctx, live, id, secret, true); err != nil {
		return err
	}

	if err := h.c.admin(deleted).DeleteAccessKey(ctx, adminapi.DeleteAccessKeyParams{AccessKey: id}); err != nil {
		return fmt.Errorf("delete %s through %s: %w", id, deleted.id, err)
	}

	return h.everyNode(ctx, live, id, secret, false)
}

// everyNode waits until every node accepts (or refuses) the key.
func (h *harness) everyNode(ctx context.Context, nodes []*node, id, secret string, accepted bool) error {
	for _, n := range nodes {
		client, err := minio.New(fmt.Sprintf("127.0.0.1:%d", n.s3), &minio.Options{
			Creds: credentials.NewStaticV4(id, secret, ""),
		})
		if err != nil {
			return err
		}

		deadline := time.Now().Add(keyDeadline)

		for {
			_, err := client.ListBuckets(ctx)
			if (err == nil) == accepted {
				break
			}

			if time.Now().After(deadline) {
				state := "refused"
				if !accepted {
					state = "still accepted"
				}

				return fmt.Errorf("%s %s by %s after %s", id, state, n.id, keyDeadline)
			}

			time.Sleep(time.Second)
		}
	}

	return nil
}

// keptNoncurrent is how many noncurrent versions the lifecycle rules keep of
// a key of worker w: odd workers' keys keep two, even workers' none.
func keptNoncurrent(w int) int {
	if w%2 == 1 {
		return 2
	}

	return 0
}

// lifecycleRules expires every noncurrent version in the versioned bucket a
// day after it was replaced (at once, given the nodes' moved clock), keeping
// the newest keptNoncurrent, and removes delete markers left with nothing
// under them.
func lifecycleRules(workers int) *lifecycle.Configuration {
	cfg := lifecycle.NewConfiguration()

	for w := range workers {
		cfg.Rules = append(cfg.Rules, lifecycle.Rule{
			ID:         fmt.Sprintf("w%d", w),
			Status:     "Enabled",
			RuleFilter: lifecycle.Filter{Prefix: fmt.Sprintf("w%d/", w)},
			Expiration: lifecycle.Expiration{DeleteMarker: true},
			NoncurrentVersionExpiration: lifecycle.NoncurrentVersionExpiration{
				NoncurrentDays:          1,
				NewerNoncurrentVersions: keptNoncurrent(w),
			},
		})
	}

	return cfg
}

// lifecycleWithin bounds how long the sweeps may take, with every node up, to
// bring each key to what its rule keeps: a restarted node waits a minute
// before its first pass.
const lifecycleWithin = 5 * time.Minute

// checkLifecycle waits until no key of the versioned bucket holds more
// noncurrent versions than its rule keeps, nor a delete marker with nothing
// under it.
func (h *harness) checkLifecycle(ctx context.Context, workers int) error {
	n := h.c.live()[0]
	deadline := time.Now().Add(lifecycleWithin)

	for {
		var (
			excess          []string
			keys, nKept     int
			markers, lonely int
		)

		for w := range workers {
			kept := keptNoncurrent(w)
			noncurrent := map[string]int{}
			versions := map[string]int{}
			marker := map[string]bool{}

			for obj := range n.client.ListObjects(ctx, "versioned", minio.ListObjectsOptions{
				Prefix: fmt.Sprintf("w%d/", w), Recursive: true, WithVersions: true,
			}) {
				if obj.Err != nil {
					return obj.Err
				}

				versions[obj.Key]++

				switch {
				case !obj.IsLatest:
					noncurrent[obj.Key]++
				case obj.IsDeleteMarker:
					marker[obj.Key] = true
				}
			}

			keys += len(versions)

			for key, c := range noncurrent {
				nKept += c

				if c > kept {
					excess = append(excess, fmt.Sprintf("%s holds %d noncurrent, keeps %d", key, c, kept))
				}
			}

			for key := range marker {
				markers++

				if versions[key] == 1 {
					excess = append(excess, key+" is a lone delete marker")
				}
			}
		}

		// A deleted key of a rule that keeps nothing is one whose marker the
		// sweeps had to remove: without any, that path went untried.
		for w, ks := range h.keys {
			for _, k := range ks {
				if k.bucket == "versioned" && keptNoncurrent(w) == 0 && len(k.possible) == 1 && k.possible[""] {
					lonely++
				}
			}
		}

		if len(excess) == 0 {
			fmt.Printf("lifecycle: %d keys listed, %d noncurrent versions kept, %d current delete markers; %d deleted keys lost their marker\n",
				keys, nKept, markers, lonely)

			return nil
		}

		if time.Now().After(deadline) {
			slices.Sort(excess)

			return fmt.Errorf("after %s: %d keys over their rule, first %s", lifecycleWithin, len(excess), excess[0])
		}

		time.Sleep(10 * time.Second)
	}
}

// usageWithin bounds how long bucket usage may take to agree with a listing
// once every node is up: replicas converge on the anti-entropy interval.
func usageWithin() time.Duration {
	if realistic {
		return 15 * time.Minute
	}

	return time.Minute
}

// checkUsage requires every bucket's usage, as the admin API reports it, to
// match the versions a listing finds: delete markers and uploads in flight
// count in neither.
func (h *harness) checkUsage(ctx context.Context) error {
	deadline := time.Now().Add(usageWithin())

	for _, b := range buckets {
		for {
			live := h.c.live()
			n := live[h.rng.IntN(len(live))]

			var listed adminapi.QuotaUsage

			var listErr error

			for obj := range n.client.ListObjects(ctx, b.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
				if obj.Err != nil {
					listErr = obj.Err

					break
				}

				if !obj.IsDeleteMarker {
					listed.Objects++
					listed.Bytes += obj.Size
				}
			}

			st, err := h.c.admin(n).GetBucketQuota(ctx, adminapi.GetBucketQuotaParams{Bucket: b.name})
			if err == nil && listErr == nil && st.Usage == listed {
				break
			}

			if time.Now().After(deadline) {
				if err == nil {
					err = listErr
				}

				if err != nil {
					return fmt.Errorf("%s: %w", b.name, err)
				}

				return fmt.Errorf("%s: usage %d objects, %d bytes; listing %d objects, %d bytes",
					b.name, st.Usage.Objects, st.Usage.Bytes, listed.Objects, listed.Bytes)
			}

			time.Sleep(3 * time.Second)
		}
	}

	return nil
}

// report prints the violations recorded so far, with each key's history: on
// a run that fails partway as much as on one that finishes.
func (h *harness) report() {
	h.vmu.Lock()
	defer h.vmu.Unlock()

	for _, v := range h.violations {
		fmt.Printf("VIOLATION round %d (%s): %s/%s read %q, expected one of %q\n",
			v.round, v.phase, v.bucket, v.key, v.observed, v.possible)

		for _, line := range v.history {
			fmt.Println("    " + line)
		}
	}
}
