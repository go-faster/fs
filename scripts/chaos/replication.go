//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/go-faster/fs/adminapi"
)

// Geo-replication under chaos (docs/design/geo-replication.md, phase 6): the
// source's versioned bucket replicates to a second, three-node deployment
// through a proxy that injects network faults — latency, a bandwidth cap,
// connection resets, outages — while a target node may be killed and
// versions are deleted at the target behind the source's back. After the
// last round and a drain:
//
//   - every version the source holds is at the target, with its ETag,
//     unless the target deleted it;
//   - no version deleted at the target came back;
//   - no version was refused;
//   - the source reports every version COMPLETED.

// replicate runs the replication target; -replication=false leaves it out.
var replicate bool

// proxyPort is where the source reaches the target.
const proxyPort = 18990

// replicaBucket is the source bucket replicated, and its name at the target.
const replicaBucket = "versioned"

// netMode is what the proxy does to the traffic through it.
type netMode int

const (
	netClean netMode = iota
	// netSlow delays every chunk, as a long round trip does.
	netSlow
	// netThrottled caps the bandwidth.
	netThrottled
	// netResets resets a connection now and then, mid-request.
	netResets
	// netDown refuses connections and cuts those open: an outage.
	netDown
)

func (m netMode) String() string {
	return [...]string{"clean", "slow", "throttled", "resets", "down"}[m]
}

// proxy forwards TCP connections to the target's live nodes, with faults.
type proxy struct {
	ln  net.Listener
	dst *cluster

	mu    sync.Mutex
	mode  netMode
	rng   *rand.Rand
	conns map[net.Conn]bool
}

func startProxy(dst *cluster, seed uint64) (*proxy, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		return nil, err
	}

	p := &proxy{ln: ln, dst: dst, rng: rand.New(rand.NewPCG(seed, 2)), conns: map[net.Conn]bool{}} //nolint:gosec // Reproducible faults.
	go p.serve()

	return p, nil
}

// set changes the mode; going down cuts every connection open.
func (p *proxy) set(m netMode) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.mode = m

	if m == netDown {
		for c := range p.conns {
			_ = c.Close()
		}
	}
}

func (p *proxy) current() netMode {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.mode
}

func (p *proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}

		go p.handle(c)
	}
}

func (p *proxy) handle(c net.Conn) {
	live := p.dst.live()

	p.mu.Lock()
	down := p.mode == netDown || len(live) == 0

	var n *node
	if !down {
		n = live[p.rng.IntN(len(live))]
	}
	p.mu.Unlock()

	if down {
		_ = c.Close()

		return
	}

	up, err := net.DialTimeout("tcp", n.s3Addr(), 5*time.Second)
	if err != nil {
		_ = c.Close()

		return
	}

	p.mu.Lock()
	p.conns[c], p.conns[up] = true, true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.conns, c)
		delete(p.conns, up)
		p.mu.Unlock()

		_ = c.Close()
		_ = up.Close()
	}()

	done := make(chan struct{}, 2)

	go func() { p.pipe(up, c); done <- struct{}{} }()
	go func() { p.pipe(c, up); done <- struct{}{} }()

	<-done
}

// pipe copies src to dst a chunk at a time, applying the mode to each.
func (p *proxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)

	for {
		n, err := src.Read(buf)
		if n > 0 {
			switch p.current() {
			case netSlow:
				time.Sleep(40 * time.Millisecond)
			case netThrottled:
				time.Sleep(time.Duration(n) * time.Second / (1 << 20)) // 1 MiB/s.
			case netResets:
				p.mu.Lock()
				reset := p.rng.IntN(100) == 0
				p.mu.Unlock()

				if reset {
					for _, c := range []net.Conn{src, dst} {
						if tc, ok := c.(*net.TCPConn); ok {
							_ = tc.SetLinger(0) // RST, not FIN.
						}
					}

					return
				}
			case netDown:
				return
			case netClean:
			}

			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}

		if err != nil {
			return
		}
	}
}

// replication is the target side of the soak.
type replication struct {
	src, dst *cluster
	proxy    *proxy
	rng      *rand.Rand

	mu sync.Mutex
	// deleted are versions deleted at the target, key@versionID.
	deleted map[string]bool
}

// newTarget is the target deployment: three nodes, t1..t3.
func newTarget(bin, dir string) (*cluster, error) {
	dst := &cluster{bin: bin, secret: secret + "-replica", widths: []int{3}}

	for i, zone := range []string{"a", "b", "c"} {
		n, err := newNode(fmt.Sprintf("t%d", i+1), zone, 18700+i, 18800+i, 18900+i, dir)
		if err != nil {
			return nil, err
		}

		n.member = true
		dst.nodes = append(dst.nodes, n)
	}

	return dst, nil
}

// startTarget starts the target deployment with its versioned bucket.
func startTarget(ctx context.Context, dst *cluster) error {
	for _, n := range dst.nodes {
		if err := dst.start(n); err != nil {
			return err
		}
	}

	if err := dst.applyLayout(ctx); err != nil {
		return fmt.Errorf("target: %w", err)
	}

	t1 := dst.nodes[0]

	if err := t1.client.MakeBucket(ctx, replicaBucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("target bucket: %w", err)
	}

	if err := t1.client.EnableVersioning(ctx, replicaBucket); err != nil {
		return fmt.Errorf("target versioning: %w", err)
	}

	return nil
}

// setup starts the target deployment, unless it runs on another machine
// (-target), and the proxy, and replicates the source's versioned bucket to
// it.
func setupReplication(ctx context.Context, src, dst *cluster, seed uint64) (*replication, error) {
	if targetHost == "" {
		if err := startTarget(ctx, dst); err != nil {
			return nil, err
		}
	}

	p, err := startProxy(dst, seed)
	if err != nil {
		return nil, err
	}

	// The proxy passes TLS through: the runner verifies a remote target's
	// certificate, made for 127.0.0.1 as well as its host.
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	if targetHost != "" {
		endpoint = fmt.Sprintf("https://127.0.0.1:%d", proxyPort)
	}

	_, err = src.admin(src.live()[0]).SetBucketReplication(ctx, &adminapi.Replication{Targets: []adminapi.ReplicationTarget{{
		Name:            "dr",
		Endpoint:        endpoint,
		Bucket:          replicaBucket,
		AccessKeyID:     accessKey,
		SecretAccessKey: adminapi.NewOptString(secretKey),
		DeleteMarkers:   adminapi.NewOptBool(true),
		Insecure:        adminapi.NewOptBool(targetHost == ""),
	}}}, adminapi.SetBucketReplicationParams{Bucket: replicaBucket})
	if err != nil {
		return nil, fmt.Errorf("set replication: %w", err)
	}

	where := "t1..t3"
	if targetHost != "" {
		where += " at " + targetHost
	}

	fmt.Printf("  %s replicates to %s through a fault proxy on :%d\n", replicaBucket, where, proxyPort)

	return &replication{
		src: src, dst: dst, proxy: p,
		rng:     rand.New(rand.NewPCG(seed, 3)), //nolint:gosec // A seeded, reproducible workload.
		deleted: map[string]bool{},
	}, nil
}

// disrupt runs the round's fault until ctx is done: a degraded link for the
// whole round (slow, throttled, resets), an outage, or a target node killed,
// in turn.
func (r *replication) disrupt(ctx context.Context, round int, load time.Duration) {
	faults := []struct {
		mode         netMode
		outage, kill bool
	}{{mode: netSlow}, {mode: netThrottled}, {mode: netResets}, {outage: true}, {kill: true}}
	f := faults[round%len(faults)]

	defer r.proxy.set(netClean)

	switch {
	case f.kill && targetHost != "":
		// A remote target's nodes are not the harness's to kill: the
		// round runs over the WAN as it is.
		fmt.Println("  replication link: clean")
	case f.outage:
		fmt.Printf("  replication link: down for %s\n", load)

		if sleep(ctx, load/2) {
			r.proxy.set(netDown)
			sleep(ctx, load)
		}
	case f.kill:
		if sleep(ctx, load/2) {
			n := r.dst.nodes[(round/len(faults))%len(r.dst.nodes)]
			fmt.Printf("  replication target: SIGKILL %s\n", n.id)
			r.dst.kill(n)
			sleep(ctx, load)

			if err := r.dst.start(n); err != nil {
				fmt.Printf("  restart %s: %v\n", n.id, err)
			}
		}
	default:
		fmt.Printf("  replication link: %s\n", f.mode)
		r.proxy.set(f.mode)
	}

	<-ctx.Done()
}

// watch samples the replication status until ctx is done, and prints the
// round's worst lag and backlog: what the runner's batch, concurrency and
// timeouts are tuned against.
func (r *replication) watch(ctx context.Context) {
	var (
		lag           float64
		backlog, size int64
		errs, samples int
	)

	for sleep(ctx, 2*time.Second) {
		live := r.src.live()
		if len(live) == 0 {
			continue
		}

		st, err := r.src.admin(live[r.rng.IntN(len(live))]).GetBucketReplicationStatus(ctx,
			adminapi.GetBucketReplicationStatusParams{Bucket: replicaBucket})
		if err != nil {
			errs++

			continue
		}

		samples++
		backlog, size = max(backlog, st.Backlog), max(size, st.BacklogBytes)

		for _, t := range st.Targets {
			lag = max(lag, t.LagSeconds)
		}
	}

	fmt.Printf("  replication lag: max %.1fs, backlog max %d versions (%.1f MiB); %d samples, %d failed\n",
		lag, backlog, float64(size)/(1<<20), samples, errs)
}

// sleep waits d or until ctx is done, and reports whether d passed.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// deleteAtTarget deletes a version at the target now and then, as its
// operator might: replication must never bring one back.
func (r *replication) deleteAtTarget(ctx context.Context, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 4)) //nolint:gosec // A seeded, reproducible workload.

	for sleep(ctx, 3*time.Second) {
		live := r.dst.live()
		if len(live) == 0 {
			continue
		}

		n := live[rng.IntN(len(live))]

		var versions []minio.ObjectInfo

		for obj := range n.client.ListObjects(ctx, replicaBucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if obj.Err != nil {
				break
			}

			if !obj.IsDeleteMarker {
				versions = append(versions, obj)
			}
		}

		if len(versions) == 0 {
			continue
		}

		v := versions[rng.IntN(len(versions))]

		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := n.client.RemoveObject(opCtx, replicaBucket, v.Key, minio.RemoveObjectOptions{VersionID: v.VersionID})

		cancel()

		if err == nil {
			r.mu.Lock()
			r.deleted[v.Key+"@"+v.VersionID] = true
			r.mu.Unlock()
		}
	}
}

// drainWithin bounds how long the source may take to ship its backlog once
// the faults stop.
func drainWithin() time.Duration {
	if realistic {
		return 15 * time.Minute
	}

	return 3 * time.Minute
}

// check drains the backlog and compares the two deployments.
func (r *replication) check(ctx context.Context) error {
	r.proxy.set(netClean)

	for _, n := range r.dst.nodes {
		n.mu.Lock()
		up := n.up
		n.mu.Unlock()

		if !up && targetHost == "" {
			if err := r.dst.start(n); err != nil {
				return err
			}
		}
	}

	start := time.Now()
	deadline := start.Add(drainWithin())

	for {
		live := r.src.live()

		st, err := r.src.admin(live[r.rng.IntN(len(live))]).GetBucketReplicationStatus(ctx,
			adminapi.GetBucketReplicationStatusParams{Bucket: replicaBucket})
		if err == nil && st.Backlog == 0 {
			fmt.Printf("  replication drained in %s\n", time.Since(start).Round(time.Second))

			break
		}

		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("drain: %w", err)
			}

			for _, t := range st.Targets {
				for _, rf := range t.RecentRefusals {
					fmt.Printf("  refused %s@%s: %s\n", rf.Key, rf.VersionID, rf.Reason)
				}
			}

			return fmt.Errorf("backlog of %d versions not drained within %s", st.Backlog, drainWithin())
		}

		time.Sleep(3 * time.Second)
	}

	srcVersions, err := versionsOf(ctx, r.src)
	if err != nil {
		return fmt.Errorf("list source: %w", err)
	}

	dstVersions, err := versionsOf(ctx, r.dst)
	if err != nil {
		return fmt.Errorf("list target: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var problems []string

	for id, etag := range srcVersions {
		got, ok := dstVersions[id]

		switch {
		case r.deleted[id]:
		case !ok:
			problems = append(problems, "missing at the target: "+id)
		case got != etag:
			problems = append(problems, fmt.Sprintf("%s: ETag %s at the target, %s at the source", id, got, etag))
		}
	}

	for id := range r.deleted {
		if _, ok := dstVersions[id]; ok {
			problems = append(problems, "deleted at the target and back: "+id)
		}
	}

	// Every version the source holds reports COMPLETED.
	statuses := map[string]int{}

	for id := range srcVersions {
		key, vid := splitID(id)
		live := r.src.live()

		info, err := live[r.rng.IntN(len(live))].client.StatObject(ctx, replicaBucket, key, minio.StatObjectOptions{VersionID: vid})
		if err != nil {
			continue // Expired by lifecycle meanwhile.
		}

		statuses[info.ReplicationStatus]++

		if info.ReplicationStatus != "COMPLETED" {
			problems = append(problems, fmt.Sprintf("%s: x-amz-replication-status %q", id, info.ReplicationStatus))
		}
	}

	fmt.Printf("  replication: %d versions at the source, %d at the target, %d deleted at the target; statuses %v\n",
		len(srcVersions), len(dstVersions), len(r.deleted), statuses)

	// The faults must have reached the runner, or the soak proved nothing
	// about recovering from them.
	failing, recovered := 0, 0

	for _, n := range r.src.nodes {
		log, err := os.ReadFile(filepath.Join(n.dir, "server.log"))
		if err != nil {
			continue
		}

		failing += bytes.Count(log, []byte("Replication target failing"))
		recovered += bytes.Count(log, []byte("Replication target recovered"))
	}

	fmt.Printf("  replication runner: target failing %d times, recovered %d times\n", failing, recovered)

	if failing == 0 {
		problems = append(problems, "the faults never made the target fail: nothing was tested")
	}

	for _, p := range problems {
		fmt.Println("  REPLICATION " + p)
	}

	if len(problems) > 0 {
		return fmt.Errorf("%d problems", len(problems))
	}

	return nil
}

// versionsOf lists the replicated bucket's versions (delete markers aside)
// through a live node of c, as key@versionID → ETag.
func versionsOf(ctx context.Context, c *cluster) (map[string]string, error) {
	live := c.live()
	if len(live) == 0 {
		return nil, errors.New("no live node")
	}

	out := map[string]string{}

	for obj := range live[0].client.ListObjects(ctx, replicaBucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}

		if !obj.IsDeleteMarker {
			out[obj.Key+"@"+obj.VersionID] = obj.ETag
		}
	}

	return out, nil
}

func splitID(id string) (key, versionID string) {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '@' {
			return id[:i], id[i+1:]
		}
	}

	return id, ""
}
