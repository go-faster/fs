package adminhandler

import (
	"context"
	"net/http"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/adminapi"
)

// ErrRebalanceConflict marks an invalid rebalance transition (starting a
// running rebalance, pausing an idle one); it maps to 409.
var ErrRebalanceConflict = errors.New("invalid rebalance transition")

// RebalanceState is the node-local runner state.
type RebalanceState string

// Runner states; see the admin API schema for semantics.
const (
	RebalanceIdle    RebalanceState = "idle"
	RebalanceWaiting RebalanceState = "waiting"
	RebalanceRunning RebalanceState = "running"
	RebalancePaused  RebalanceState = "paused"
	RebalanceDone    RebalanceState = "done"
	RebalanceFailed  RebalanceState = "failed"
)

// RebalanceStatus is a snapshot of the node's rebalance runner.
type RebalanceStatus struct {
	State RebalanceState
	// Objects, Relocated and Failed are the current or last run's progress.
	Objects, Relocated, Failed int
	// CursorBucket/CursorKey is the persisted resume cursor (empty when none).
	CursorBucket, CursorKey string
	// StartedAt/FinishedAt frame the current or last run; zero when unset.
	StartedAt, FinishedAt time.Time
	// Err is why the last run failed.
	Err string
	// RepairQueueDepth is the node's pending async remainder backlog.
	RepairQueueDepth int
}

// RebalanceControl drives the node's cluster rebalance runner. Implemented by
// the cluster runtime; absent (nil) outside cluster mode.
type RebalanceControl interface {
	// Start launches the rebalance (campaigning for the cluster-wide slot).
	// Returns ErrRebalanceConflict when one is already waiting or running.
	Start(ctx context.Context) error
	// Pause stops this node's runner, keeping the resume cursor. Returns
	// ErrRebalanceConflict when nothing is waiting or running.
	Pause(ctx context.Context) error
	// Resume relaunches a paused rebalance from the persisted cursor. Returns
	// ErrRebalanceConflict unless the runner is paused.
	Resume(ctx context.Context) error
	// Status snapshots the runner.
	Status(ctx context.Context) RebalanceStatus
}

// GetRebalanceStatus reports the node's rebalance runner state.
func (a *AdminAPI) GetRebalanceStatus(ctx context.Context) (*adminapi.RebalanceStatus, error) {
	if a.opts.Rebalance == nil {
		return &adminapi.RebalanceStatus{State: adminapi.RebalanceStateDisabled}, nil
	}

	return statusToAPI(a.opts.Rebalance.Status(ctx)), nil
}

// ControlRebalance starts, pauses or resumes the rebalance runner.
func (a *AdminAPI) ControlRebalance(ctx context.Context, req *adminapi.RebalanceControlRequest) (*adminapi.RebalanceStatus, error) {
	ctl := a.opts.Rebalance
	if ctl == nil {
		return nil, apiErr(http.StatusConflict, errors.New("not in cluster mode"))
	}

	var err error

	switch req.Action {
	case adminapi.RebalanceActionStart:
		err = ctl.Start(ctx)
	case adminapi.RebalanceActionPause:
		err = ctl.Pause(ctx)
	case adminapi.RebalanceActionResume:
		err = ctl.Resume(ctx)
	default:
		return nil, apiErr(http.StatusBadRequest, errors.Errorf("unknown action %q", req.Action))
	}

	if err != nil {
		if errors.Is(err, ErrRebalanceConflict) {
			return nil, apiErr(http.StatusConflict, err)
		}

		return nil, apiErr(http.StatusInternalServerError, err)
	}

	return statusToAPI(ctl.Status(ctx)), nil
}

// statusToAPI maps a runner snapshot to the wire schema.
func statusToAPI(s RebalanceStatus) *adminapi.RebalanceStatus {
	out := &adminapi.RebalanceStatus{
		State:            adminapi.RebalanceState(s.State),
		Objects:          s.Objects,
		Relocated:        s.Relocated,
		Failed:           s.Failed,
		RepairQueueDepth: s.RepairQueueDepth,
	}

	if s.CursorBucket != "" || s.CursorKey != "" {
		out.CursorBucket = adminapi.NewOptString(s.CursorBucket)
		out.CursorKey = adminapi.NewOptString(s.CursorKey)
	}

	if !s.StartedAt.IsZero() {
		out.StartedAt = adminapi.NewOptDateTime(s.StartedAt)
	}

	if !s.FinishedAt.IsZero() {
		out.FinishedAt = adminapi.NewOptDateTime(s.FinishedAt)
	}

	if s.Err != "" {
		out.ErrorMessage = adminapi.NewOptString(s.Err)
	}

	return out
}
