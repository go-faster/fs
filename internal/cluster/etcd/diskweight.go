package etcd

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/cluster"
)

// Disk weight overrides live outside the node registry, and that separation is
// the whole point.
//
// A node republishes its own registry key — identity, rack, disks, capacity —
// on every usage refresh, so a weight written into that key by anyone else is
// gone within thirty seconds. An override is therefore its own key, owned by
// whoever set it, and the topology source merges the two: the node says which
// disks exist, the override says how much placement should use them.
//
// It is what makes a drain an API call instead of a config edit and a restart
// (fs SPEC §11.6). The override survives the node restarting, because it was
// never the node's to forget.

// overridesPrefix is the key namespace for per-disk weight overrides.
func (c Config) overridesPrefix() string { return c.Prefix + "/overrides/disks/" }

// diskWeightKey is one disk's override key.
func (c Config) diskWeightKey(node cluster.NodeID, disk cluster.DiskID) string {
	return c.overridesPrefix() + string(node) + "/" + string(disk)
}

// diskWeightRecord is the JSON wire form of an override.
//
// Weight is a pointer so that zero is a value and not an absence. That is the
// trap the config file has — an omitted weight and an explicit 0 are the same
// there, so 0 is read as "unset" and becomes 1 — and an override whose whole
// purpose is to drain a disk must be able to say zero and mean it.
type diskWeightRecord struct {
	Weight *float64 `json:"weight"`
	// Reason is free text from whoever set the override, carried back on
	// reads so an operator finding a drained disk can see why.
	Reason string `json:"reason,omitempty"`
}

// DiskWeight is one disk's override as stored.
type DiskWeight struct {
	Node   cluster.NodeID
	Disk   cluster.DiskID
	Weight float64
	Reason string
}

// Drained reports whether the override takes the disk out of placement, which
// is what a weight that is not positive means everywhere in fs.
func (d DiskWeight) Drained() bool { return d.Weight <= 0 }

// SetDiskWeight overrides a disk's placement weight until it is cleared.
//
// It does not check that the disk exists: a node may be down when its disk is
// drained, and refusing then would mean the one moment an operator most wants
// to take a disk out of placement is the moment they cannot. An override for a
// disk that never appears is inert.
func SetDiskWeight(
	ctx context.Context, client *clientv3.Client, cfg Config,
	node cluster.NodeID, disk cluster.DiskID, weight float64, reason string,
) error {
	cfg = cfg.withDefaults()

	if node == "" || disk == "" {
		return errors.New("node and disk are required")
	}

	data, err := json.Marshal(diskWeightRecord{Weight: &weight, Reason: reason})
	if err != nil {
		return errors.Wrap(err, "marshal disk weight")
	}

	if _, err := client.Put(ctx, cfg.diskWeightKey(node, disk), string(data)); err != nil {
		return errors.Wrap(err, "put disk weight")
	}

	return nil
}

// ClearDiskWeight removes an override, restoring the weight the node
// registers. Clearing an absent override is not an error: the caller wanted
// the disk back at its configured weight, and it is.
func ClearDiskWeight(
	ctx context.Context, client *clientv3.Client, cfg Config,
	node cluster.NodeID, disk cluster.DiskID,
) error {
	cfg = cfg.withDefaults()

	if _, err := client.Delete(ctx, cfg.diskWeightKey(node, disk)); err != nil {
		return errors.Wrap(err, "delete disk weight")
	}

	return nil
}

// ListDiskWeights returns every override currently set.
func ListDiskWeights(ctx context.Context, client *clientv3.Client, cfg Config) ([]DiskWeight, error) {
	cfg = cfg.withDefaults()

	resp, err := client.Get(ctx, cfg.overridesPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, errors.Wrap(err, "list disk weights")
	}

	out := make([]DiskWeight, 0, len(resp.Kvs))

	for _, kv := range resp.Kvs {
		override, ok := decodeDiskWeight(cfg, string(kv.Key), kv.Value)
		if !ok {
			continue
		}

		out = append(out, override)
	}

	return out, nil
}

// decodeDiskWeight parses one override key and value. A malformed record is
// skipped rather than fatal: a bad key must not take placement down, and the
// disk simply keeps its registered weight.
func decodeDiskWeight(cfg Config, key string, value []byte) (DiskWeight, bool) {
	node, disk, ok := splitDiskWeightKey(cfg, key)
	if !ok {
		return DiskWeight{}, false
	}

	var rec diskWeightRecord
	if err := json.Unmarshal(value, &rec); err != nil || rec.Weight == nil {
		return DiskWeight{}, false
	}

	return DiskWeight{Node: node, Disk: disk, Weight: *rec.Weight, Reason: rec.Reason}, true
}

// splitDiskWeightKey recovers the node and disk from an override key.
func splitDiskWeightKey(cfg Config, key string) (cluster.NodeID, cluster.DiskID, bool) {
	rest, ok := strings.CutPrefix(key, cfg.overridesPrefix())
	if !ok {
		return "", "", false
	}

	node, disk, ok := strings.Cut(rest, "/")
	if !ok || node == "" || disk == "" {
		return "", "", false
	}

	return cluster.NodeID(node), cluster.DiskID(disk), true
}
