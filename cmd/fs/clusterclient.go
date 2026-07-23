package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// clusterClient is a disk-less cluster client for operator commands: it
// watches the topology and talks to the nodes over the authenticated peer
// transport under a synthetic node ID, without registering in the cluster.
type clusterClient struct {
	client  *clientv3.Client
	coord   *clusterstore.Coordinator
	etcdCfg etcd.Config
	self    cluster.NodeID

	closers []func() error
}

// validateClusterClientConfig checks the config fields operator commands need.
func validateClusterClientConfig(cfg Config) error {
	if len(cfg.Cluster.Etcd.Endpoints) == 0 {
		return errors.New("cluster.etcd.endpoints is required (pass the node config via --config)")
	}

	if len(cfg.ClusterSecret()) < 16 {
		return errors.New("cluster.secret (or FS_CLUSTER_SECRET) is required, min 16 characters")
	}

	return nil
}

// dialClusterClient builds the client; label names the command in the
// synthetic node ID. dialer, when non-nil, wraps the default peer dialer
// (bandwidth throttling).
func dialClusterClient(ctx context.Context, cfg Config, label string, wrap func(clusterstore.PeerDialer) clusterstore.PeerDialer) (*clusterClient, error) {
	cc := cfg.Cluster

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   cc.Etcd.Endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, errors.Wrap(err, "etcd client")
	}

	c := &clusterClient{client: client, closers: []func() error{client.Close}}

	c.etcdCfg = etcd.Config{Prefix: cc.Etcd.Prefix}
	if cc.Etcd.TTL > 0 {
		c.etcdCfg.TTL = int64(cc.Etcd.TTL / time.Second)
	}

	source, err := etcd.NewSource(ctx, client, c.etcdCfg)
	if err != nil {
		_ = c.Close()
		return nil, errors.Wrap(err, "watch topology")
	}

	c.closers = append(c.closers, source.Close)

	defaultScheme := scheme.Default
	if cc.Scheme != "" {
		if defaultScheme, err = scheme.Parse(cc.Scheme); err != nil {
			_ = c.Close()
			return nil, errors.Wrap(err, "cluster.scheme")
		}
	}

	// The synthetic ID is never registered, so it can't collide with a
	// topology node and never resolves to the nil local store.
	host, _ := os.Hostname()
	c.self = cluster.NodeID(fmt.Sprintf("%s/%s/%d", label, host, os.Getpid()))

	var dialer clusterstore.PeerDialer = clusterstore.NewHTTPPeers(c.self, nil, transport.Secret(cfg.ClusterSecret()), nil)
	if wrap != nil {
		dialer = wrap(dialer)
	}

	c.coord, err = clusterstore.New(clusterstore.Config{
		Topology: source,
		Peers:    dialer,
		Scheme:   func(string) scheme.Scheme { return defaultScheme },
	})
	if err != nil {
		_ = c.Close()
		return nil, errors.Wrap(err, "cluster coordinator")
	}

	c.closers = append(c.closers, c.coord.Close)

	return c, nil
}

// Close tears the client down in reverse construction order.
func (c *clusterClient) Close() error {
	var firstErr error

	for i := len(c.closers) - 1; i >= 0; i-- {
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	c.closers = nil

	return firstErr
}
