package adminhandler

import (
	"context"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
)

// DiskWeight is one disk's placement-weight override as the control plane
// stores it.
type DiskWeight struct {
	Node   string
	Disk   string
	Weight float64
	Reason string
}

// Drained reports whether the override takes the disk out of placement, which
// is what a weight that is not positive means everywhere in fs.
func (d DiskWeight) Drained() bool { return d.Weight <= 0 }

// DiskWeightStore reads and writes per-disk placement weight overrides.
//
// An override lives outside the node's registration on purpose: a node
// republishes its own record on every capacity refresh, so a weight written
// there by anyone else would be gone within the refresh interval — and a
// drained disk would silently return to placement (fs SPEC §11.6).
//
// nil outside cluster mode; the endpoints then return 501.
type DiskWeightStore interface {
	ListDiskWeights(ctx context.Context) ([]DiskWeight, error)
	SetDiskWeight(ctx context.Context, node, disk string, weight float64, reason string) error
	ClearDiskWeight(ctx context.Context, node, disk string) error
}

// ListDiskWeights returns every override currently set.
func (a *AdminAPI) ListDiskWeights(ctx context.Context) (*adminapi.DiskWeightList, error) {
	if a.opts.DiskWeights == nil {
		return nil, a.errNoDiskWeights()
	}

	overrides, err := a.opts.DiskWeights.ListDiskWeights(ctx)
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, err)
	}

	out := &adminapi.DiskWeightList{Overrides: make([]adminapi.DiskWeight, 0, len(overrides))}
	for _, override := range overrides {
		out.Overrides = append(out.Overrides, diskWeightToAPI(override))
	}

	return out, nil
}

// SetDiskWeight overrides one disk's placement weight.
func (a *AdminAPI) SetDiskWeight(
	ctx context.Context, req *adminapi.SetDiskWeightRequest, params adminapi.SetDiskWeightParams,
) (*adminapi.DiskWeight, error) {
	if a.opts.DiskWeights == nil {
		return nil, a.errNoDiskWeights()
	}

	if params.Node == "" || params.Disk == "" {
		return nil, apiErr(http.StatusBadRequest, errors.New("node and disk are required"))
	}

	reason := req.Reason.Or("")

	if err := a.opts.DiskWeights.SetDiskWeight(ctx, params.Node, params.Disk, req.Weight, reason); err != nil {
		return nil, apiErr(http.StatusInternalServerError, err)
	}

	stored := DiskWeight{Node: params.Node, Disk: params.Disk, Weight: req.Weight, Reason: reason}

	return ptrTo(diskWeightToAPI(stored)), nil
}

// ClearDiskWeight restores the weight the node registers.
func (a *AdminAPI) ClearDiskWeight(ctx context.Context, params adminapi.ClearDiskWeightParams) error {
	if a.opts.DiskWeights == nil {
		return a.errNoDiskWeights()
	}

	if err := a.opts.DiskWeights.ClearDiskWeight(ctx, params.Node, params.Disk); err != nil {
		return apiErr(http.StatusInternalServerError, err)
	}

	return nil
}

// diskWeightToAPI maps the domain type to the wire schema.
func diskWeightToAPI(d DiskWeight) adminapi.DiskWeight {
	out := adminapi.DiskWeight{
		Node:    d.Node,
		Disk:    d.Disk,
		Weight:  d.Weight,
		Drained: adminapi.NewOptBool(d.Drained()),
	}

	if d.Reason != "" {
		out.Reason = adminapi.NewOptString(d.Reason)
	}

	return out
}

// ptrTo is the address of a value, for returning a generated struct by
// pointer.
func ptrTo[T any](v T) *T { return &v }

func (a *AdminAPI) errNoDiskWeights() *adminapi.ErrorStatusCode {
	return apiErr(http.StatusNotImplemented,
		errors.New("disk weight overrides are not available on this admin listener"))
}
