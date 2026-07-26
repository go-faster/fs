package main

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// diskWeightSource adapts the etcd control plane to
// adminhandler.DiskWeightStore. It backs the disk-weight endpoints on both the
// per-node admin and the headless `fs admin`: an override is control-plane
// state, so either process can set one and every node sees it (fs SPEC §11.6).
type diskWeightSource struct {
	client *clientv3.Client
	cfg    etcd.Config
}

var _ adminhandler.DiskWeightStore = diskWeightSource{}

// newDiskWeightSource builds the store, or nil when there is no control plane
// — which is what makes the endpoints report 501 outside cluster mode.
func newDiskWeightSource(client *clientv3.Client, cfg etcd.Config) adminhandler.DiskWeightStore {
	if client == nil {
		return nil
	}

	return diskWeightSource{client: client, cfg: cfg}
}

func (s diskWeightSource) ListDiskWeights(ctx context.Context) ([]adminhandler.DiskWeight, error) {
	overrides, err := etcd.ListDiskWeights(ctx, s.client, s.cfg)
	if err != nil {
		return nil, err
	}

	out := make([]adminhandler.DiskWeight, 0, len(overrides))
	for _, o := range overrides {
		out = append(out, adminhandler.DiskWeight{
			Node:   string(o.Node),
			Disk:   string(o.Disk),
			Weight: o.Weight,
			Reason: o.Reason,
		})
	}

	return out, nil
}

func (s diskWeightSource) SetDiskWeight(
	ctx context.Context, node, disk string, weight float64, reason string,
) error {
	return etcd.SetDiskWeight(ctx, s.client, s.cfg,
		cluster.NodeID(node), cluster.DiskID(disk), weight, reason)
}

func (s diskWeightSource) ClearDiskWeight(ctx context.Context, node, disk string) error {
	return etcd.ClearDiskWeight(ctx, s.client, s.cfg, cluster.NodeID(node), cluster.DiskID(disk))
}
