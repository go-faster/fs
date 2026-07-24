package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
	"github.com/go-faster/fs/storagefs"
)

// clusterRuntime is a running cluster node: its fs.Storage backend, the peer
// listener to serve, and the teardown chain.
type clusterRuntime struct {
	// Storage is the replicated fs.Storage backend for the S3 server.
	Storage fs.Storage

	server    *http.Server
	repairer  *clusterstore.Repairer
	rebalance *rebalanceController
	listener  net.Listener
	addr      string
	lg        *zap.Logger
	closers   []func() error
	coord     *clusterstore.Coordinator
	nodeID    cluster.NodeID
	schemeID  string

	// Usage refresh inputs: the local disk store, this node's static registry
	// identity and its registration handle.
	store *diskstore.Store
	node  cluster.Node
	reg   *etcd.Registration

	// scrub accumulates this node's scrub totals for metrics.
	scrub scrubTotals
}

// scrubTotals are cumulative scrub counters, updated by RunScrubber and read
// by the metrics callback.
type scrubTotals struct {
	passes, objects, repaired, failed  atomic.Int64
	rebuilt, sweptStale, corrupt       atomic.Int64
	converted, rewrittenSidecars       atomic.Int64
	unknownDirs, ecUnverifiedLastScrub atomic.Int64
}

// observe folds one scrub pass in.
func (s *scrubTotals) observe(report *clusterstore.ScrubReport) {
	s.passes.Add(1)
	s.objects.Add(int64(report.Objects))
	s.repaired.Add(int64(report.Repaired))
	s.failed.Add(int64(report.Failed))
	s.rebuilt.Add(int64(report.Totals.RebuiltFragments))
	s.sweptStale.Add(int64(report.Totals.DeletedStale))
	s.corrupt.Add(int64(report.Totals.CorruptReplicas))
	s.converted.Add(int64(report.Totals.Converted))
	s.rewrittenSidecars.Add(int64(report.Totals.RewrittenSidecars))
	s.unknownDirs.Add(int64(report.UnknownDirs))

	ec := int64(0)
	if report.Totals.ECUnverified {
		ec = 1
	}

	s.ecUnverifiedLastScrub.Store(ec)
}

// buildCluster wires a cluster node from config: disk stores, etcd
// registration + topology source, the coordinator and the peer transport
// server. absRoot anchors the default disk layout.
func buildCluster(ctx context.Context, lg *zap.Logger, cfg Config, absRoot string) (*clusterRuntime, error) {
	cc := cfg.Cluster

	syncPolicy, err := storagefs.ParseSyncPolicy(cfg.Storage.Fsync)
	if err != nil {
		return nil, errors.Wrap(err, "storage fsync policy")
	}

	defaultScheme := scheme.Default
	if cc.Scheme != "" {
		if defaultScheme, err = scheme.Parse(cc.Scheme); err != nil {
			return nil, errors.Wrap(err, "cluster.scheme")
		}
	}

	// Disk roots: configured, or one default disk under the storage root.
	const defaultDisk = "d0"

	roots := make(map[cluster.DiskID]string, len(cc.Disks))
	disks := make([]cluster.Disk, 0, max(len(cc.Disks), 1))

	if len(cc.Disks) == 0 {
		roots[defaultDisk] = filepath.Join(absRoot, "cluster", defaultDisk)
		disks = append(disks, cluster.Disk{ID: defaultDisk, Weight: 1})
	}

	for _, d := range cc.Disks {
		roots[cluster.DiskID(d.ID)] = d.Path

		weight := d.Weight
		if weight == 0 {
			weight = 1
		}

		disks = append(disks, cluster.Disk{ID: cluster.DiskID(d.ID), Weight: weight})
	}

	store, err := diskstore.New(roots, diskstore.WithSyncPolicy(syncPolicy))
	if err != nil {
		return nil, errors.Wrap(err, "cluster disk store")
	}

	rt := &clusterRuntime{
		lg:       lg,
		nodeID:   cluster.NodeID(cc.NodeID),
		schemeID: defaultScheme.String(),
	}

	// Bind the peer listener BEFORE registering in etcd: the moment the node
	// appears in the topology, peers may dial it — a registered node without
	// an accepting socket serves connection-refused to its cluster.
	addr := cc.Addr
	if addr == "" {
		addr = DefaultClusterAddr
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, errors.Wrap(err, "bind cluster listener")
	}

	rt.listener = listener
	rt.addr = listener.Addr().String()

	// Control plane: etcd client, this node's leased registration, and the
	// watched topology.
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   cc.Etcd.Endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		_ = listener.Close()
		return nil, errors.Wrap(err, "etcd client")
	}

	rt.closers = append(rt.closers, client.Close)

	etcdCfg := etcd.Config{Prefix: cc.Etcd.Prefix}
	if cc.Etcd.TTL > 0 {
		etcdCfg.TTL = int64(cc.Etcd.TTL / time.Second)
	}

	// Schema-compatibility gate: refuse to join a cluster whose on-disk/etcd
	// schema is newer than this binary understands (a stale binary must not
	// misread a migrated format). On an empty cluster this stamps the founding
	// schema version.
	clusterSchema, err := etcd.EnsureCompatible(ctx, client, etcdCfg, etcd.SchemaVersion)
	if err != nil {
		_ = listener.Close()
		_ = rt.close()

		return nil, errors.Wrap(err, "schema compatibility")
	}

	lg.Info("Cluster schema",
		zap.Int("cluster_version", clusterSchema),
		zap.Int("binary_version", etcd.SchemaVersion),
	)

	rt.store = store
	rt.node = cluster.Node{
		ID:    rt.nodeID,
		Addr:  cc.AdvertiseAddr,
		Rack:  cc.Rack,
		Disks: disks,
	}

	reg, err := etcd.Register(ctx, client, etcdCfg, rt.withUsage(rt.node))
	if err != nil {
		_ = listener.Close()
		_ = rt.close()

		return nil, errors.Wrap(err, "register node")
	}

	rt.reg = reg
	rt.closers = append(rt.closers, reg.Close)

	source, err := etcd.NewSource(ctx, client, etcdCfg)
	if err != nil {
		_ = listener.Close()
		_ = rt.close()

		return nil, errors.Wrap(err, "watch topology")
	}

	source.OnError = func(err error) {
		lg.Warn("Cluster topology watch error", zap.Error(err))
	}

	rt.closers = append(rt.closers, source.Close)

	secret := transport.Secret(cfg.ClusterSecret())

	coord, err := clusterstore.New(clusterstore.Config{
		Topology: source,
		Peers:    clusterstore.NewHTTPPeers(rt.nodeID, store, secret, nil),
		Scheme:   func(string) scheme.Scheme { return defaultScheme },
		OnAsyncError: func(bucket, key string, err error) {
			lg.Warn("Async replication remainder failed (repair will complete it)",
				zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
		},
	})
	if err != nil {
		_ = listener.Close()
		_ = rt.close()

		return nil, errors.Wrap(err, "cluster coordinator")
	}

	rt.coord = coord
	rt.closers = append(rt.closers, coord.Close)
	rt.Storage = clusterstore.NewStorage(coord)

	rt.repairer, err = clusterstore.NewRepairer(clusterstore.RepairerConfig{
		Coordinator: coord,
		Self:        rt.nodeID,
		Verify:      true,
		OnError: func(bucket, key string, err error) {
			lg.Warn("Object repair failed",
				zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
		},
	})
	if err != nil {
		_ = listener.Close()
		_ = rt.close()

		return nil, errors.Wrap(err, "cluster repairer")
	}

	// The admin API's rebalance runner: same elected, cursor-checkpointed walk
	// as `fs cluster rebalance`, using this node's repairer. Its runs are
	// bounded by ctx (the server lifetime).
	rt.rebalance = newRebalanceController(ctx, lg, client, etcdCfg, coord, rt.repairer, string(rt.nodeID)+"/admin")

	rt.server = &http.Server{
		Handler:           transport.NewServer(store, secret),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return rt, nil
}

// Serve runs the peer replication listener until ctx is canceled, then drains
// it and tears the node down (deregistering it from the cluster).
func (rt *clusterRuntime) Serve(ctx context.Context) error {
	rt.lg.Info("Starting cluster listener",
		zap.String("addr", rt.addr),
		zap.String("node", string(rt.nodeID)),
		zap.String("scheme", rt.schemeID),
	)

	errCh := make(chan error, 1)

	go func() {
		if err := rt.server.Serve(rt.listener); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}

		errCh <- nil
	}()

	select {
	case err := <-errCh:
		_ = rt.close()
		return errors.Wrap(err, "cluster listener")
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := rt.server.Shutdown(shutdownCtx)

	if cerr := rt.close(); err == nil {
		err = cerr
	}

	return err
}

// usageRefreshInterval is how often a node re-publishes its per-disk
// capacity to the registry.
const usageRefreshInterval = 30 * time.Second

// defaultFullWatermark is the disk-fullness fraction beyond which the node
// warns that the disk needs draining.
const defaultFullWatermark = 0.9

// withUsage returns the node with current per-disk capacity filled in;
// unreadable filesystems report as unknown (0/0).
func (rt *clusterRuntime) withUsage(node cluster.Node) cluster.Node {
	out := node
	out.Disks = make([]cluster.Disk, len(node.Disks))

	for i, d := range node.Disks {
		out.Disks[i] = d

		if u, err := rt.store.Usage(d.ID); err == nil {
			out.Disks[i].TotalBytes = u.TotalBytes
			out.Disks[i].FreeBytes = u.FreeBytes
		}
	}

	return out
}

// RunUsageReporter periodically re-publishes this node's registry record with
// fresh per-disk capacity (ROADMAP Phase 9 cluster metrics), and warns when a
// local disk crosses the fullness watermark — with deterministic weighted
// placement, a persistently full disk needs a weight change (drain), which
// the auto-rebalancer then converges.
func (rt *clusterRuntime) RunUsageReporter(ctx context.Context, watermark float64) {
	if watermark <= 0 || watermark > 1 {
		watermark = defaultFullWatermark
	}

	ticker := time.NewTicker(usageRefreshInterval)
	defer ticker.Stop()

	warned := make(map[cluster.DiskID]bool, len(rt.node.Disks))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		node := rt.withUsage(rt.node)

		if err := rt.reg.Update(ctx, node); err != nil {
			if ctx.Err() == nil {
				rt.lg.Warn("Publishing disk usage failed", zap.Error(err))
			}

			continue
		}

		for _, d := range node.Disks {
			if d.TotalBytes == 0 {
				continue
			}

			full := 1 - float64(d.FreeBytes)/float64(d.TotalBytes)

			if over := full >= watermark; over != warned[d.ID] {
				warned[d.ID] = over

				if over {
					rt.lg.Warn("Disk crossed the fullness watermark; consider lowering its weight (drain) — auto-rebalance converges after a weight change",
						zap.String("disk", string(d.ID)),
						zap.Float64("fullness", full),
						zap.Float64("watermark", watermark),
					)
				} else {
					rt.lg.Info("Disk back under the fullness watermark",
						zap.String("disk", string(d.ID)),
						zap.Float64("fullness", full),
					)
				}
			}
		}
	}
}

// RunScrubber periodically walks this node's disks and repairs every object
// found — the cluster-wide scrub/repair loop (checksum-verifying). A no-op
// when interval is zero.
func (rt *clusterRuntime) RunScrubber(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	rt.lg.Info("Cluster scrubber enabled", zap.Duration("interval", interval))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		report, err := rt.repairer.Scrub(ctx)
		if err != nil {
			if ctx.Err() == nil {
				rt.lg.Warn("Cluster scrub pass failed", zap.Error(err))
			}

			continue
		}

		rt.scrub.observe(report)

		log := rt.lg.Debug
		if report.Repaired > 0 || report.Failed > 0 || report.Totals.ECUnverified {
			log = rt.lg.Warn
		}

		log("Cluster scrub pass",
			zap.Int("objects", report.Objects),
			zap.Int("repaired", report.Repaired),
			zap.Int("failed", report.Failed),
			zap.Int("rebuilt_fragments", report.Totals.RebuiltFragments),
			zap.Int("rewritten_sidecars", report.Totals.RewrittenSidecars),
			zap.Int("deleted_stale", report.Totals.DeletedStale),
			zap.Int("corrupt_replicas", report.Totals.CorruptReplicas),
			zap.Int("unknown_dirs", report.UnknownDirs),
			zap.Bool("ec_unverified", report.Totals.ECUnverified),
		)
	}
}

// close tears down the node in reverse construction order: coordinator (async
// queue drained), topology watch, registration (lease revoked — the node
// leaves the topology promptly), etcd client.
func (rt *clusterRuntime) close() error {
	var firstErr error

	for i := len(rt.closers) - 1; i >= 0; i-- {
		if err := rt.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	rt.closers = nil

	return firstErr
}
