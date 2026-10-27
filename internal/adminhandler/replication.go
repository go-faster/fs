package adminhandler

import (
	"context"
	"net/http"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/replication"
)

const (
	// replicationCheckTimeout bounds the handshake with each target.
	replicationCheckTimeout = 10 * time.Second
	// statusRows bounds the outbox rows a status reads.
	statusRows = 100_000
	// statusRefusals is how many refusals a target's status lists.
	statusRefusals = 20
)

// GetBucketReplication returns a bucket's replication targets, secrets
// omitted.
func (a *AdminAPI) GetBucketReplication(
	ctx context.Context, params adminapi.GetBucketReplicationParams,
) (*adminapi.Replication, error) {
	if a.opts.Engine == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	targets, err := a.opts.Engine.BucketReplication(ctx, params.Bucket)
	if err != nil {
		return nil, replicationErr(err)
	}

	out := &adminapi.Replication{Targets: []adminapi.ReplicationTarget{}}

	for _, t := range targets {
		at := adminapi.ReplicationTarget{
			Name: t.Name, Endpoint: t.Endpoint, Bucket: t.Bucket, AccessKeyID: t.AccessKeyID,
		}

		if t.Prefix != "" {
			at.Prefix = adminapi.NewOptString(t.Prefix)
		}

		if t.DeleteMarkers {
			at.DeleteMarkers = adminapi.NewOptBool(true)
		}

		if t.Insecure {
			at.Insecure = adminapi.NewOptBool(true)
		}

		out.Targets = append(out.Targets, at)
	}

	return out, nil
}

// SetBucketReplication replaces a bucket's replication targets, each checked
// with the replication handshake first.
func (a *AdminAPI) SetBucketReplication(
	ctx context.Context, req *adminapi.Replication, params adminapi.SetBucketReplicationParams,
) (*adminapi.Replication, error) {
	if a.opts.Engine == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	current, err := a.opts.Engine.BucketReplication(ctx, params.Bucket)
	if err != nil {
		return nil, replicationErr(err)
	}

	secrets := make(map[string]string, len(current))
	for _, t := range current {
		secrets[t.Name] = t.SecretAccessKey
	}

	targets := make([]engine.ReplicationTarget, 0, len(req.Targets))

	for _, t := range req.Targets {
		et := engine.ReplicationTarget{
			Name: t.Name, Endpoint: t.Endpoint, Bucket: t.Bucket, AccessKeyID: t.AccessKeyID,
			SecretAccessKey: t.SecretAccessKey.Or(secrets[t.Name]),
			Prefix:          t.Prefix.Or(""),
			DeleteMarkers:   t.DeleteMarkers.Or(false),
			Insecure:        t.Insecure.Or(false),
		}

		if err := et.Validate(); err != nil {
			return nil, apiErr(http.StatusBadRequest, err)
		}

		if err := a.checkTarget(ctx, et); err != nil {
			return nil, apiErr(http.StatusBadRequest, err)
		}

		targets = append(targets, et)
	}

	if err := a.opts.Engine.SetBucketReplication(ctx, params.Bucket, targets); err != nil {
		return nil, replicationErr(err)
	}

	return a.GetBucketReplication(ctx, adminapi.GetBucketReplicationParams(params))
}

// checkTarget is the handshake with a target, bounded in time.
func (a *AdminAPI) checkTarget(ctx context.Context, t engine.ReplicationTarget) error {
	ctx, cancel := context.WithTimeout(ctx, replicationCheckTimeout)
	defer cancel()

	return a.opts.ReplicationCheck(ctx, t)
}

// DeleteBucketReplication stops replicating a bucket.
func (a *AdminAPI) DeleteBucketReplication(ctx context.Context, params adminapi.DeleteBucketReplicationParams) error {
	if a.opts.Engine == nil {
		return apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	if err := a.opts.Engine.SetBucketReplication(ctx, params.Bucket, nil); err != nil {
		return replicationErr(err)
	}

	return nil
}

// GetBucketReplicationStatus reports how far a bucket's replication has
// got: from its outbox, and from the runner when this node runs it.
func (a *AdminAPI) GetBucketReplicationStatus(
	ctx context.Context, params adminapi.GetBucketReplicationStatusParams,
) (*adminapi.ReplicationStatus, error) {
	e := a.opts.Engine
	if e == nil {
		return nil, apiErr(http.StatusNotImplemented, errors.New("no storage engine"))
	}

	targets, err := e.BucketReplication(ctx, params.Bucket)
	if err != nil {
		return nil, replicationErr(err)
	}

	backlog, err := e.ReplicationBacklog(ctx, params.Bucket)
	if err != nil {
		return nil, replicationErr(err)
	}

	byTarget, complete, err := e.ReplicationStatus(ctx, params.Bucket, statusRows, statusRefusals)
	if err != nil {
		return nil, replicationErr(err)
	}

	r := a.opts.Replication
	runs := r != nil && r.Runs(params.Bucket)

	var stats map[replication.Target]replication.TargetStats
	if runs {
		stats = r.Stats()
	}

	now := a.opts.now()
	out := &adminapi.ReplicationStatus{
		Runner:       runs,
		Complete:     complete,
		Backlog:      backlog.Objects,
		BacklogBytes: backlog.Bytes,
		Targets:      []adminapi.ReplicationTargetStatus{},
	}

	for _, t := range targets {
		tb := byTarget[t.Name]
		ts := adminapi.ReplicationTargetStatus{
			Name:           t.Name,
			Pending:        tb.Pending,
			PendingBytes:   tb.Bytes,
			Refused:        tb.Refused,
			RecentRefusals: []adminapi.ReplicationRefusal{},
		}

		if !tb.Oldest.IsZero() {
			ts.LagSeconds = max(now.Sub(tb.Oldest), 0).Seconds()
		}

		for _, rf := range tb.Recent {
			ts.RecentRefusals = append(ts.RecentRefusals, adminapi.ReplicationRefusal{
				Key: rf.Key, VersionID: rf.VersionID, Reason: rf.Reason, At: rf.At,
			})
		}

		if st, ok := stats[replication.Target{Bucket: params.Bucket, Name: t.Name}]; ok {
			ts.Failing = adminapi.NewOptBool(st.Failing)
			ts.Shipped = adminapi.NewOptInt64(st.Shipped)
			ts.BlocksOffered = adminapi.NewOptInt64(st.BlocksOffered)
			ts.BlocksSent = adminapi.NewOptInt64(st.BlocksSent)
			ts.BytesSent = adminapi.NewOptInt64(st.BytesSent)

			if !st.LastSuccess.IsZero() {
				ts.LastSuccess = adminapi.NewOptDateTime(st.LastSuccess)
			}

			if !st.LastFailure.IsZero() {
				ts.LastFailure = adminapi.NewOptDateTime(st.LastFailure)
				ts.LastError = adminapi.NewOptString(st.LastError)
			}
		}

		out.Targets = append(out.Targets, ts)
	}

	return out, nil
}

func replicationErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrBucketNotFound):
		return apiErr(http.StatusNotFound, err)
	case errors.Is(err, fs.ErrInvalidBucketState):
		return apiErr(http.StatusConflict, err)
	default:
		return err
	}
}
