package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/objindex"
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
	status    *clusterStatusSource
	migrate   *migrateController
	listener  net.Listener
	addr      string
	lg        *zap.Logger
	closers   []func() error
	coord     *clusterstore.Coordinator
	metaPlane *metadataPlane
	nodeID    cluster.NodeID
	schemeID  string
	// version and started stamp the live state this node reports to peers.
	version string
	started time.Time

	// client and etcdCfg are the control-plane handle, exposed so the S3 server
	// can back etcd-sourced credentials (auth.source: etcd) on this same node.
	client  *clientv3.Client
	etcdCfg etcd.Config

	// Usage refresh inputs: the local disk store, this node's static registry
	// identity and its registration handle.
	store *diskstore.Store
	node  cluster.Node
	reg   *etcd.Registration

	// usage batches per-bucket object accounting into the control plane. Nil
	// when the coordinator was built without it.
	usage *usageReporter

	// index is this node's metadata store: the objects its disks hold. Nothing
	// reads it yet; it is maintained so the listing, usage and scrub paths can
	// stop walking once they are moved onto it.
	//
	// It is held as the interface, not as the pebble implementation, so a
	// cluster-scope store can take its place without a caller noticing. Which
	// one is in place is asked of the store itself, via Scope.
	index metastore.Store
	// indexer feeds it from the disk store, and counts what it could not take.
	indexer *objectIndexer
	// indexAdopted is whether the previous process handed the index over
	// cleanly. Captured at construction because Open is the only moment it can
	// be known — see objindex.Index.Adopted.
	indexAdopted bool

	// scrub accumulates this node's scrub totals for metrics.
	scrub scrubTotals
	// coverage is the last computed verification coverage, recomputed in the
	// background because deriving it scans the index.
	coverage atomic.Pointer[metastore.Coverage]

	// closeOnce guards teardown. Serve tears the node down when its context
	// ends, and so does every construction error path, so close can be reached
	// twice and from two goroutines at once.
	closeOnce sync.Once
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

		disks = append(disks, cluster.Disk{ID: cluster.DiskID(d.ID), Weight: d.PlacementWeight()})
	}

	// The object index is opened before the store so the store can report
	// records to it as they land.
	index, err := objindex.Open(objindex.DefaultDir(absRoot))
	if err != nil {
		return nil, errors.Wrap(err, "cluster object index")
	}

	indexer := newObjectIndexer(index, lg)

	store, err := diskstore.New(roots,
		diskstore.WithSyncPolicy(syncPolicy),
		diskstore.WithObserver(indexer),
	)
	if err != nil {
		_ = index.Close()

		return nil, errors.Wrap(err, "cluster disk store")
	}

	build := buildInfo()

	rt := &clusterRuntime{
		lg:       lg,
		nodeID:   cluster.NodeID(cfg.ClusterNodeID()),
		schemeID: defaultScheme.String(),
		version:  build.Version,
		started:  time.Now(),
	}

	// Registered first so it closes last, after everything that might still
	// write to it — and so a failure anywhere below this line does not leave an
	// open database behind. On Windows an unclosed one cannot even be deleted.
	rt.index = index
	rt.indexAdopted = index.Adopted()
	rt.indexer = indexer
	rt.closers = append(rt.closers, index.Close)

	// Bind the peer listener BEFORE registering in etcd: the moment the node
	// appears in the topology, peers may dial it — a registered node without
	// an accepting socket serves connection-refused to its cluster.
	addr := cc.Addr
	if addr == "" {
		addr = DefaultClusterAddr
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		// rt already owns the object index, so this must go through rt.close()
		// like every failure below it. Returning bare would leave an open
		// database behind — which on Windows cannot even be deleted, the exact
		// thing the registration comment above is about.
		_ = rt.close()

		return nil, errors.Wrap(err, "bind cluster listener")
	}

	rt.listener = listener
	rt.addr = listener.Addr().String()

	// A node told to advertise port 0 advertises the port it actually bound.
	//
	// Binding is what allocates it, so nothing above this line could have
	// known it, and a registration carrying ":0" is one no peer can dial. It
	// also removes the only reason a caller would have to pick a port itself
	// and hope it is still free by the time this runs.
	advertise := resolveAdvertisePort(cfg.ClusterAdvertiseAddr(), listener.Addr())

	// Control plane: etcd client, this node's leased registration, and the
	// watched topology.
	etcdClientCfg, err := cfg.etcdClientConfig()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	client, err := clientv3.New(etcdClientCfg)
	if err != nil {
		_ = listener.Close()
		return nil, errors.Wrap(err, "etcd client")
	}

	rt.closers = append(rt.closers, client.Close)

	etcdCfg := etcd.Config{Prefix: cc.Etcd.Prefix}
	if cc.Etcd.TTL > 0 {
		etcdCfg.TTL = int64(cc.Etcd.TTL / time.Second)
	}

	rt.client = client
	rt.etcdCfg = etcdCfg

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
	// Checkpointing the occupancy index and marking the object index clean are
	// the last things the node does, so an orderly restart adopts both instead
	// of walking every disk.
	rt.closers = append(rt.closers, store.Close)
	rt.node = cluster.Node{
		ID: rt.nodeID,
		// Through the accessor, not the raw field: FS_CLUSTER_ADVERTISE_ADDR is
		// how an instance is told the address peers reach it on, and validation
		// already accepts it in place of the config value. Reading the field
		// here registered an empty address for such a node — it started,
		// reported healthy, and every peer failed to dial it.
		Addr:  advertise,
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

	rt.usage = newUsageReporter(client, etcdCfg, lg)

	keyring, err := cfg.Encryption.Keyring()
	if err != nil {
		_ = listener.Close()
		_ = rt.close()

		return nil, errors.Wrap(err, "server-side encryption")
	}

	peers := clusterstore.NewHTTPPeers(rt.nodeID, store, secret, peerHTTPClient(0)).
		WithLocalIndex(rt.indexPages)

	// The cluster-scope metadata plane, when it is turned on. Off, the node
	// keeps the per-node index and the listing merge, which is what every
	// release so far has run.
	meta := metastore.Store(index)

	if cc.Metadata.Sharded {
		plane, err := openMetadataPlane(ctx, planeDeps{
			self:     rt.nodeID,
			root:     absRoot,
			client:   client,
			etcdCfg:  etcdCfg,
			topology: source.Topology,
			peers:    peers,
		})
		if err != nil {
			_ = listener.Close()
			_ = rt.close()

			return nil, err
		}

		rt.metaPlane = plane
		rt.closers = append(rt.closers, plane.Close)

		meta = plane.Store()

		// Records land in the plane rather than in this node's own index, so
		// an entry reaches the shard that owns its key instead of the node
		// whose disk happens to hold the object.
		indexer.redirect(meta)

		// And the index it stopped writing to is no longer a description of
		// anything. Marked building so that turning the plane back off cannot
		// silently serve listings from a store that has been ignored since the
		// day it was bypassed — it costs a rebuild, which is the honest price.
		if err := index.MarkBuilding(ctx); err != nil {
			_ = listener.Close()
			_ = rt.close()

			return nil, errors.Wrap(err, "retire the local object index")
		}

		lg.Info("Sharded metadata plane enabled",
			zap.Int("ranges", cfg.MetadataRanges()),
			zap.Int("replicas", cfg.MetadataReplicas()))
	}

	coord, err := clusterstore.New(clusterstore.Config{
		Topology: source,
		Peers:    peers,
		Scheme:   func(string) scheme.Scheme { return defaultScheme },
		OnAsyncError: func(bucket, key string, err error) {
			lg.Warn("Async replication remainder failed (repair will complete it)",
				zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
		},
		Usage:   rt.usage,
		Keyring: keyring,
		// Either the node's own index — ScopeLocal, so listings merge across
		// nodes and usage sums per-node counters — or the sharded plane, which
		// reports ScopeCluster and answers a listing with one scan. The seam
		// was built for exactly this substitution; nothing else here changes.
		Metastore: meta,
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
		// Scrub cursors live on the disks they describe, so a restart resumes
		// the sweep instead of starting the disk over, and the sweep streams
		// names rather than holding every one on the disk in memory.
		ScrubState: store.ScrubStateStore(),
		Fragments:  store,
		// Verification stamps go to the object index, which is also what lets
		// the sweep stop remembering every object it has visited.
		Verification: newObjectVerifier(index, lg),
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
	rt.status = newClusterStatusSource(coord, client, etcdCfg, newPeerStatus(rt.nodeID, secret))
	rt.migrate = newMigrateController(client, etcdCfg, string(rt.nodeID)+"/admin",
		clusterMigrations(migrationDeps{client: client, etcdCfg: etcdCfg, coord: coord}))

	// The node serves its own live runtime state to peers alongside fragments:
	// it is what an admin — on any node or headless — aggregates into the
	// cluster-wide view.
	rt.server = &http.Server{
		Handler: instrumentPeerHandler(transport.NewServer(store, secret,
			append(peerServerOptions(rt),
				transport.WithStatus(rt.nodeStatus),
				transport.WithIndex(rt.indexPages),
			)...,
		)),
		ReadHeaderTimeout: 10 * time.Second,
		// Seed the peer-request contexts with this node's logger. Without it
		// zctx hands the transport a nop and everything it reports about a
		// failed peer request — including the originating request ID — is
		// written nowhere.
		BaseContext: func(net.Listener) context.Context { return zctx.Base(ctx, lg) },
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

// occupancyRescanInterval is how often the node re-anchors its per-disk
// occupancy counters against what the disks actually hold. The counters are
// maintained incrementally by the write path, so this only sheds the drift a
// scan-versus-write race can leave behind — hourly is frequent enough for a
// number an operator watches, and rare enough that the walk stays background
// noise.
const occupancyRescanInterval = time.Hour

// RunOccupancyIndex anchors this node's per-disk occupancy counters and keeps
// them honest.
//
// The first scan matters most: until it lands the counters mean nothing and the
// node reports them as such, so a drain readout says "scanning" rather than a
// confident, wrong zero. A start that adopted a clean checkpoint is already
// anchored and only re-anchors on the interval.
func (rt *clusterRuntime) RunOccupancyIndex(ctx context.Context) {
	ticker := time.NewTicker(occupancyRescanInterval)
	defer ticker.Stop()

	for {
		started := time.Now()

		if err := rt.store.ScanAll(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}

			rt.lg.Warn("Occupancy scan failed; per-disk counters stay stale until the next pass", zap.Error(err))
		} else {
			rt.lg.Debug("Occupancy scan complete", zap.Duration("took", time.Since(started)))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
// Teardown runs once. A second caller gets nil and returns only after the
// first has finished, so nothing observes a half-closed node.
//
// Without that, two callers race between reading the length of the closer list
// and the nil that follows it, and the loser indexes past the end — a panic on
// the shutdown path, which is the worst place to have one.
func (rt *clusterRuntime) close() error {
	var firstErr error

	rt.closeOnce.Do(func() {
		for i := len(rt.closers) - 1; i >= 0; i-- {
			if err := rt.closers[i](); err != nil && firstErr == nil {
				firstErr = err
			}
		}

		rt.closers = nil
	})

	return firstErr
}

// resolveAdvertisePort substitutes the bound port into an advertised address
// whose port is 0, and leaves anything else exactly as configured.
func resolveAdvertisePort(advertise string, bound net.Addr) string {
	host, port, err := net.SplitHostPort(advertise)
	if err != nil || port != "0" {
		return advertise
	}

	_, actual, err := net.SplitHostPort(bound.String())
	if err != nil {
		return advertise
	}

	return net.JoinHostPort(host, actual)
}
