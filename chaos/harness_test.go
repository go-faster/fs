//go:build chaos

// Package chaos is the ROADMAP Phase 9 chaos suite: it builds the real fs
// binary, runs a multi-node cluster as separate OS processes against an
// embedded (restartable) etcd, applies scripted faults — node SIGKILL and
// restart, disk wipe, node add, etcd restart — under continuous mixed-scheme
// S3 load, and gates on the durability invariants:
//
//   - no acked write is lost (every acknowledged PUT reads back as a payload
//     acknowledged for that key at or after the last ack), and
//   - no under-protected object after convergence (every fragment of every
//     object present, at the right size, at the current epoch's placement).
//
// The suite is behind the "chaos" build tag and driven by its own CI
// workflow (.github/workflows/chaos.yml).
package chaos

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/server/v3/embed"
)

// binPath is the fs binary built once in TestMain.
var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fs-chaos-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	binPath = filepath.Join(dir, "fs")

	build := exec.Command("go", "build", "-o", binPath, "github.com/go-faster/fs/cmd/fs")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr

	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build fs binary:", err)
		os.Exit(1)
	}

	code := m.Run()

	_ = os.RemoveAll(dir)

	os.Exit(code)
}

// freeAddr reserves a listen address. The listener is closed immediately;
// chaos steps tolerate the tiny reuse race.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

// etcdServer is the embedded control plane, restartable on the same data dir
// and URLs to simulate an etcd outage.
type etcdServer struct {
	dir       string
	clientURL url.URL
	peerURL   url.URL
	srv       *embed.Etcd
}

func startEtcd(t *testing.T) *etcdServer {
	t.Helper()

	e := &etcdServer{
		dir:       t.TempDir(),
		clientURL: url.URL{Scheme: "http", Host: freeAddr(t)},
		peerURL:   url.URL{Scheme: "http", Host: freeAddr(t)},
	}

	e.start(t)
	t.Cleanup(e.stop)

	return e
}

func (e *etcdServer) start(t *testing.T) {
	t.Helper()

	cfg := embed.NewConfig()
	cfg.Dir = e.dir
	cfg.LogLevel = "error"
	cfg.ListenClientUrls = []url.URL{e.clientURL}
	cfg.AdvertiseClientUrls = []url.URL{e.clientURL}
	cfg.ListenPeerUrls = []url.URL{e.peerURL}
	cfg.AdvertisePeerUrls = []url.URL{e.peerURL}
	cfg.InitialCluster = cfg.InitialClusterFromName(cfg.Name)

	srv, err := embed.StartEtcd(cfg)
	require.NoError(t, err)

	select {
	case <-srv.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		t.Fatal("etcd did not become ready")
	}

	e.srv = srv
}

func (e *etcdServer) stop() {
	if e.srv != nil {
		e.srv.Close()
		e.srv = nil
	}
}

// restart simulates an etcd outage: hard stop, then a fresh server on the
// same data dir and URLs.
func (e *etcdServer) restart(t *testing.T) {
	t.Helper()

	e.stop()
	e.start(t)
}

// clusterSecret authenticates peer traffic across the chaos cluster.
const clusterSecret = "chaos-secret-0123456789abcdef"

// etcdPrefix namespaces the chaos cluster in etcd.
const etcdPrefix = "/fs-chaos"

// node is one fs server process.
type node struct {
	id          string
	s3Addr      string
	clusterAddr string
	root        string
	diskRoot    string
	configPath  string
	logPath     string

	cmd *exec.Cmd
	log *os.File
}

// newNode writes the node's config; the process starts with start().
func newNode(t *testing.T, i int, etcdURL string) *node {
	t.Helper()

	n := &node{
		id:          fmt.Sprintf("n%d", i),
		s3Addr:      freeAddr(t),
		clusterAddr: freeAddr(t),
		root:        t.TempDir(),
	}
	n.diskRoot = filepath.Join(n.root, "disk0")
	n.configPath = filepath.Join(n.root, "config.yaml")
	n.logPath = filepath.Join(n.root, "server.log")

	cfg := fmt.Sprintf(`server:
  addr: %q
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 60s
  health_path: /health
storage:
  root: %q
  type: cluster
  fsync: none
auth:
  disabled: true
integrity:
  scrub_interval: 5s
observability:
  service_name: fs-chaos
  enable_request_logging: false
  enable_metrics: false
  enable_tracing: false
cluster:
  node_id: %q
  rack: rack-%s
  addr: %q
  advertise_addr: %q
  secret: %q
  scheme: "rf2.5"
  disks:
    - id: d0
      path: %q
  etcd:
    endpoints: [%q]
    prefix: %q
    ttl: 2s
  rebalance:
    settle: 2s
    cooldown: 2s
`, n.s3Addr, n.root, n.id, n.id, n.clusterAddr, n.clusterAddr, clusterSecret, n.diskRoot, etcdURL, etcdPrefix)

	require.NoError(t, os.WriteFile(n.configPath, []byte(cfg), 0o644))

	return n
}

// start launches the server process, appending to the node's log.
func (n *node) start(t *testing.T) {
	t.Helper()

	log, err := os.OpenFile(n.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)

	n.log = log

	cmd := exec.Command(binPath, "s3", "--config", n.configPath)
	cmd.Stdout, cmd.Stderr = log, log

	require.NoError(t, cmd.Start())

	n.cmd = cmd

	t.Cleanup(func() { n.kill() })
}

// kill SIGKILLs the process (crash semantics: no dereg, the etcd lease
// expires on its own). Idempotent.
func (n *node) kill() {
	if n.cmd == nil {
		return
	}

	_ = n.cmd.Process.Kill()
	_ = n.cmd.Wait()

	n.cmd = nil

	if n.log != nil {
		_ = n.log.Close()
		n.log = nil
	}
}

// waitHealthy blocks until the node's S3 listener answers its health check.
func (n *node) waitHealthy(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for {
		resp, err := http.Get("http://" + n.s3Addr + "/health")
		if err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("node %s did not become healthy; log: %s", n.id, n.logPath)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// runCLI executes an fs subcommand (e.g. `cluster scheme`) against the
// cluster using the node's config.
func (n *node) runCLI(t *testing.T, args ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, append(args, "--config", n.configPath)...)

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "fs %v: %s", args, out)
}
