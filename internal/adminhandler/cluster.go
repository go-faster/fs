package adminhandler

import (
	"context"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/internal/cluster/layout"
)

func (a *AdminAPI) errNoCluster() *adminapi.ErrorStatusCode {
	return apiErr(http.StatusNotImplemented, errors.New("cluster mode is off"))
}

// GetLayout summarizes the adopted layout.
func (a *AdminAPI) GetLayout(_ context.Context) (*adminapi.Layout, error) {
	if a.opts.Cluster == nil {
		return nil, a.errNoCluster()
	}

	l := a.opts.Cluster.Layout()
	if l == nil {
		return nil, apiErr(http.StatusNotFound, errors.New("no layout has been applied"))
	}

	out := layoutToAPI(l)

	return &out, nil
}

// ApplyLayout computes the next layout from the requested roles and, unless
// it is a dry run, adopts it.
func (a *AdminAPI) ApplyLayout(
	ctx context.Context, req *adminapi.ApplyLayoutRequest, params adminapi.ApplyLayoutParams,
) (*adminapi.LayoutChange, error) {
	if a.opts.Cluster == nil {
		return nil, a.errNoCluster()
	}

	nodes := make([]layout.Node, 0, len(req.Members))
	for _, m := range req.Members {
		nodes = append(nodes, layout.Node{
			ID:       layout.NodeID(m.ID),
			Zone:     m.Zone.Or(""),
			Rack:     m.Rack.Or(""),
			Capacity: m.Capacity,
		})
	}

	dryRun := params.DryRun.Or(false)
	opts := layout.Options{Partitions: req.Partitions.Or(0), Widths: req.Widths}

	// A layout narrower than a bucket's erasure code would strand its
	// shards: refuse it before anything is adopted.
	if a.opts.Engine != nil {
		next, _, err := a.opts.Cluster.Apply(nodes, opts, true)
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, err)
		}

		if err := a.opts.Engine.CheckLayout(ctx, next); err != nil {
			return nil, apiErr(http.StatusBadRequest, err)
		}
	}

	next, moved, err := a.opts.Cluster.Apply(nodes, opts, dryRun)
	if err != nil {
		return nil, apiErr(http.StatusBadRequest, err)
	}

	return &adminapi.LayoutChange{Layout: layoutToAPI(next), MovedSlots: moved, Applied: !dryRun}, nil
}

// ListClusterNodes reports this node and its peers.
func (a *AdminAPI) ListClusterNodes(_ context.Context) (*adminapi.ClusterNodeList, error) {
	if a.opts.Cluster == nil {
		return nil, a.errNoCluster()
	}

	self := a.opts.Cluster.Status()
	nodes := []adminapi.ClusterNode{{
		ID:            adminapi.NewOptString(string(self.ID)),
		Addr:          self.Addr,
		Self:          true,
		Up:            true,
		LayoutVersion: adminapi.NewOptUint64(self.Version),
	}}

	for _, p := range a.opts.Cluster.Peers() {
		n := adminapi.ClusterNode{Addr: p.Addr, Up: p.Err == nil && !p.Seen.IsZero()}

		if p.ID != "" {
			n.ID = adminapi.NewOptString(string(p.ID))
			n.LayoutVersion = adminapi.NewOptUint64(p.Version)
		}

		if !p.Seen.IsZero() {
			n.LastSeen = adminapi.NewOptDateTime(p.Seen)
		}

		if p.Err != nil {
			n.Error = adminapi.NewOptString(p.Err.Error())
		}

		nodes = append(nodes, n)
	}

	return &adminapi.ClusterNodeList{Nodes: nodes}, nil
}

func layoutToAPI(l *layout.Layout) adminapi.Layout {
	load := l.Load()

	out := adminapi.Layout{Version: l.Version, Partitions: len(l.Slots), Widths: l.Widths}

	for _, n := range l.Nodes {
		m := adminapi.LayoutMember{ID: string(n.ID), Capacity: n.Capacity, Slots: load[n.ID]}
		if n.Zone != "" {
			m.Zone = adminapi.NewOptString(n.Zone)
		}

		if n.Rack != "" {
			m.Rack = adminapi.NewOptString(n.Rack)
		}

		out.Members = append(out.Members, m)
	}

	for _, w := range l.Widths {
		zone, rack := l.MaxPerDomain(w)
		out.Spread = append(out.Spread, adminapi.LayoutSpread{Width: w, MaxPerZone: zone, MaxPerRack: rack})
	}

	return out
}
