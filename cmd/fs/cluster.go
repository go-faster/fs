package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
)

// DefaultClusterAddr is the default peer listener address.
const DefaultClusterAddr = ":7080"

// ClusterConfig configures cluster membership. Setting NodeID turns it on.
//
// Membership only, for now: nodes gossip and agree on a layout, but objects
// are still stored on the node that received them (#279).
type ClusterConfig struct {
	// NodeID is this node's identity in the layout. Unique per node; the
	// FS_CLUSTER_NODE_ID environment variable takes precedence.
	NodeID string `yaml:"node_id,omitempty"`

	// Addr is the peer listener bind address (default DefaultClusterAddr).
	// Internal: never expose it publicly. Peer traffic is authenticated, not
	// encrypted.
	Addr string `yaml:"addr,omitempty"`

	// AdvertiseAddr is the host:port peers reach this node at. Required; the
	// FS_CLUSTER_ADVERTISE_ADDR environment variable takes precedence.
	AdvertiseAddr string `yaml:"advertise_addr,omitempty"`

	// Peers are host:port addresses of nodes to join through. One is enough:
	// the rest are learned by gossip.
	Peers []string `yaml:"peers,omitempty"`

	// Secret authenticates peer traffic; the same on every node, at least 16
	// characters. The FS_CLUSTER_SECRET environment variable takes precedence.
	Secret string `yaml:"secret,omitempty"`
}

// clusterEnabled reports whether cluster mode is on.
func (c *Config) clusterEnabled() bool { return c.clusterNodeID() != "" }

func (c *Config) clusterNodeID() string {
	return envOr("FS_CLUSTER_NODE_ID", c.Cluster.NodeID)
}

func (c *Config) clusterAdvertiseAddr() string {
	return envOr("FS_CLUSTER_ADVERTISE_ADDR", c.Cluster.AdvertiseAddr)
}

func (c *Config) clusterSecret() string {
	return envOr("FS_CLUSTER_SECRET", c.Cluster.Secret)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func (c *Config) validateCluster() error {
	if !c.clusterEnabled() {
		return nil
	}

	if c.clusterAdvertiseAddr() == "" {
		return errors.New("cluster.advertise_addr (or FS_CLUSTER_ADVERTISE_ADDR) is required")
	}

	if len(c.clusterSecret()) < 16 {
		return errors.New("cluster.secret (or FS_CLUSTER_SECRET) is required, at least 16 characters")
	}

	return nil
}

// startCluster joins the cluster: it serves the peer protocol, gossips until
// ctx is canceled and exports membership metrics. It returns nil when cluster
// mode is off.
func startCluster(
	ctx context.Context, lg *zap.Logger, cfg Config, root string, mp metric.MeterProvider, serve func(func() error),
) (*peer.Member, error) {
	if !cfg.clusterEnabled() {
		return nil, nil
	}

	m, err := peer.New(peer.Config{
		ID:     layout.NodeID(cfg.clusterNodeID()),
		Addr:   cfg.clusterAdvertiseAddr(),
		Peers:  cfg.Cluster.Peers,
		Secret: peer.Secret(cfg.clusterSecret()),
		Dir:    filepath.Join(root, ".cluster"),
	})
	if err != nil {
		return nil, errors.Wrap(err, "cluster membership")
	}

	if err := registerClusterMetrics(mp, m); err != nil {
		return nil, err
	}

	addr := cfg.Cluster.Addr
	if addr == "" {
		addr = DefaultClusterAddr
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           m.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	lg.Warn("Cluster membership is on; objects are not replicated yet (#279)",
		zap.String("node_id", cfg.clusterNodeID()),
		zap.String("addr", addr),
		zap.String("advertise_addr", cfg.clusterAdvertiseAddr()),
	)

	serve(func() error {
		go m.Run(ctx)

		go func() { //nolint:gosec // Detached shutdown context is intentional: ctx is already canceled here.
			<-ctx.Done()

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			_ = srv.Shutdown(shutdownCtx)
		}()

		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.Wrap(err, "cluster listen and serve")
		}

		return nil
	})

	return m, nil
}

// registerClusterMetrics exports the adopted layout version — compare it
// across nodes to see gossip falling behind — and how many peers answer.
func registerClusterMetrics(mp metric.MeterProvider, m *peer.Member) error {
	meter := mp.Meter("github.com/go-faster/fs/cluster")

	version, err := meter.Int64ObservableGauge("fs.cluster.layout.version",
		metric.WithDescription("Version of the layout this node has adopted; 0 before the first."))
	if err != nil {
		return errors.Wrap(err, "layout version gauge")
	}

	peers, err := meter.Int64ObservableGauge("fs.cluster.peers",
		metric.WithDescription("Known peers by whether the last exchange with them succeeded."))
	if err != nil {
		return errors.Wrap(err, "peers gauge")
	}

	up, down := attribute.String("state", "up"), attribute.String("state", "down")

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		var v int64
		if l := m.Layout(); l != nil {
			v = int64(l.Version) //nolint:gosec // Versions count applies; they never reach 2^63.
		}

		o.ObserveInt64(version, v)

		var nUp, nDown int64

		for _, p := range m.Peers() {
			if p.Err == nil && !p.Seen.IsZero() {
				nUp++
			} else {
				nDown++
			}
		}

		o.ObserveInt64(peers, nUp, metric.WithAttributes(up))
		o.ObserveInt64(peers, nDown, metric.WithAttributes(down))

		return nil
	}, version, peers)
	if err != nil {
		return errors.Wrap(err, "register cluster metrics")
	}

	return nil
}
