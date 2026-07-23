// Package etcd is the go-faster/fs cluster control plane: the only component
// that talks to etcd. Nodes announce themselves with lease-bound registry
// keys (ID, address, rack, per-disk weights), and every node watches the
// registry into an epoch-stamped cluster.Topology snapshot — the pure input
// the placement function and clusterstore consume via
// clusterstore.TopologySource. Everything downstream stays testable without
// etcd (StaticTopology in clusterstore); this package's integration tests run
// a real in-process etcd.
//
// A node that dies or partitions loses its lease and drops out of the
// topology after the TTL; the epoch is the etcd revision, so placement is
// stable within an epoch and both sides of a topology change are computable.
package etcd

import (
	"context"
	"encoding/json"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// DefaultTTL is the default registration lease TTL in seconds: how long a
// dead node keeps lingering in the topology.
const DefaultTTL = 10

// DefaultPrefix is the default etcd key namespace.
const DefaultPrefix = "/fs"

// Config configures registration and the topology source.
type Config struct {
	// Prefix namespaces all cluster keys (default DefaultPrefix).
	Prefix string
	// TTL is the registration lease TTL in seconds (default DefaultTTL,
	// minimum 1). A node's registry key survives at most this long past its
	// last keepalive.
	TTL int64
}

// withDefaults resolves zero-value config fields.
func (c Config) withDefaults() Config {
	if c.Prefix == "" {
		c.Prefix = DefaultPrefix
	}

	if c.TTL <= 0 {
		c.TTL = DefaultTTL
	}

	return c
}

// nodesPrefix is the registry key namespace under the config prefix.
func (c Config) nodesPrefix() string { return c.Prefix + "/nodes/" }

// nodeKey is one node's registry key.
func (c Config) nodeKey(id cluster.NodeID) string { return c.nodesPrefix() + string(id) }

// nodeRecord is the JSON wire form of a registered node.
type nodeRecord struct {
	ID    cluster.NodeID `json:"id"`
	Addr  string         `json:"addr"`
	Rack  string         `json:"rack,omitempty"`
	Disks []diskRecord   `json:"disks"`
}

type diskRecord struct {
	ID     cluster.DiskID `json:"id"`
	Weight float64        `json:"weight"`
	// Total/Free report the disk's filesystem capacity (0 = unknown),
	// refreshed by Registration.Update.
	Total uint64 `json:"total,omitempty"`
	Free  uint64 `json:"free,omitempty"`
}

// encodeNode marshals a node for its registry key.
func encodeNode(n cluster.Node) ([]byte, error) {
	rec := nodeRecord{ID: n.ID, Addr: n.Addr, Rack: n.Rack, Disks: make([]diskRecord, 0, len(n.Disks))}
	for _, d := range n.Disks {
		rec.Disks = append(rec.Disks, diskRecord{ID: d.ID, Weight: d.Weight, Total: d.TotalBytes, Free: d.FreeBytes})
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return nil, errors.Wrap(err, "marshal node record")
	}

	return data, nil
}

// decodeNode parses a registry value.
func decodeNode(data []byte) (cluster.Node, error) {
	var rec nodeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return cluster.Node{}, errors.Wrap(err, "unmarshal node record")
	}

	n := cluster.Node{ID: rec.ID, Addr: rec.Addr, Rack: rec.Rack, Disks: make([]cluster.Disk, 0, len(rec.Disks))}
	for _, d := range rec.Disks {
		n.Disks = append(n.Disks, cluster.Disk{ID: d.ID, Weight: d.Weight, TotalBytes: d.Total, FreeBytes: d.Free})
	}

	if n.ID == "" {
		return cluster.Node{}, errors.New("node record without ID")
	}

	return n, nil
}

// contextDone reports whether ctx has been canceled.
func contextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
