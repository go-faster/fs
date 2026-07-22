package main

import (
	"context"
	"net/http"
	"path/filepath"
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

	server   *http.Server
	addr     string
	lg       *zap.Logger
	closers  []func() error
	coord    *clusterstore.Coordinator
	nodeID   cluster.NodeID
	schemeID string
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

	// Control plane: etcd client, this node's leased registration, and the
	// watched topology.
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   cc.Etcd.Endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, errors.Wrap(err, "etcd client")
	}

	rt.closers = append(rt.closers, client.Close)

	etcdCfg := etcd.Config{Prefix: cc.Etcd.Prefix}
	if cc.Etcd.TTL > 0 {
		etcdCfg.TTL = int64(cc.Etcd.TTL / time.Second)
	}

	reg, err := etcd.Register(ctx, client, etcdCfg, cluster.Node{
		ID:    rt.nodeID,
		Addr:  cc.AdvertiseAddr,
		Rack:  cc.Rack,
		Disks: disks,
	})
	if err != nil {
		_ = rt.close()
		return nil, errors.Wrap(err, "register node")
	}

	rt.closers = append(rt.closers, reg.Close)

	source, err := etcd.NewSource(ctx, client, etcdCfg)
	if err != nil {
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
		_ = rt.close()
		return nil, errors.Wrap(err, "cluster coordinator")
	}

	rt.coord = coord
	rt.closers = append(rt.closers, coord.Close)
	rt.Storage = clusterstore.NewStorage(coord)

	addr := cc.Addr
	if addr == "" {
		addr = DefaultClusterAddr
	}

	rt.addr = addr
	rt.server = &http.Server{
		Addr:              addr,
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
		if err := rt.server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
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
