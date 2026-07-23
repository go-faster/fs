package adminhandler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminapi"
)

// stubRebalance scripts RebalanceControl responses.
type stubRebalance struct {
	status  RebalanceStatus
	err     error
	actions []string
}

func (s *stubRebalance) Start(context.Context) error {
	s.actions = append(s.actions, "start")
	return s.err
}

func (s *stubRebalance) Pause(context.Context) error {
	s.actions = append(s.actions, "pause")
	return s.err
}

func (s *stubRebalance) Resume(context.Context) error {
	s.actions = append(s.actions, "resume")
	return s.err
}
func (s *stubRebalance) Status(context.Context) RebalanceStatus {
	return s.status
}

func newRebalanceAPI(t *testing.T, ctl RebalanceControl) *AdminAPI {
	t.Helper()

	mgr, err := auth.NewManager(auth.Config{Keys: []auth.Key{
		{AccessKey: "AKIACONFIG", SecretKey: "config-secret", Grants: []auth.Grant{{Pattern: "*", Permission: auth.Admin}}},
	}}, "")
	require.NoError(t, err)

	return NewAdminAPI(Options{Manager: mgr, Rebalance: ctl})
}

func TestRebalanceDisabledOutsideClusterMode(t *testing.T) {
	a := newRebalanceAPI(t, nil)

	st, err := a.GetRebalanceStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, adminapi.RebalanceStateDisabled, st.State)

	_, err = a.ControlRebalance(t.Context(), &adminapi.RebalanceControlRequest{Action: adminapi.RebalanceActionStart})
	requireStatusCode(t, err, http.StatusConflict)
}

func TestRebalanceControlActions(t *testing.T) {
	stub := &stubRebalance{status: RebalanceStatus{
		State:            RebalanceRunning,
		Objects:          7,
		Relocated:        3,
		Failed:           1,
		CursorBucket:     "b",
		CursorKey:        "k5",
		StartedAt:        time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		RepairQueueDepth: 2,
	}}

	a := newRebalanceAPI(t, stub)

	for _, action := range []adminapi.RebalanceAction{
		adminapi.RebalanceActionStart, adminapi.RebalanceActionPause, adminapi.RebalanceActionResume,
	} {
		st, err := a.ControlRebalance(t.Context(), &adminapi.RebalanceControlRequest{Action: action})
		require.NoError(t, err, action)
		assert.Equal(t, adminapi.RebalanceStateRunning, st.State)
	}

	assert.Equal(t, []string{"start", "pause", "resume"}, stub.actions)

	st, err := a.GetRebalanceStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 7, st.Objects)
	assert.Equal(t, 3, st.Relocated)
	assert.Equal(t, 1, st.Failed)
	assert.Equal(t, "b", st.CursorBucket.Or(""))
	assert.Equal(t, "k5", st.CursorKey.Or(""))
	assert.Equal(t, 2, st.RepairQueueDepth)
	assert.True(t, st.StartedAt.Set)
	assert.False(t, st.FinishedAt.Set)
	assert.False(t, st.ErrorMessage.Set)
}

func TestRebalanceControlConflict(t *testing.T) {
	stub := &stubRebalance{err: ErrRebalanceConflict}
	a := newRebalanceAPI(t, stub)

	_, err := a.ControlRebalance(t.Context(), &adminapi.RebalanceControlRequest{Action: adminapi.RebalanceActionPause})
	requireStatusCode(t, err, http.StatusConflict)
}

// requireStatusCode asserts an ogen structured error's HTTP status.
func requireStatusCode(t *testing.T, err error, want int) {
	t.Helper()

	var sc *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &sc)
	assert.Equal(t, want, sc.StatusCode)
}
