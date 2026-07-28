package main

import (
	"context"

	"github.com/go-faster/errors"
	"go.uber.org/zap"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/metastore"
)

// RunMetaRebuild keeps a cluster-scope metadata store built.
//
// At local scope each node rebuilds its own index from its own disks, which is
// what RunObjectIndex does and needs no coordination. A cluster-scope store
// describes objects no single node holds, so it cannot be rebuilt that way: it
// becomes one cooperative walk of the whole cluster, with one elected runner
// and a cursor so a killed runner does not start over.
//
// This is the piece that ties the two halves together — the walk
// (clusterstore.RebuildMetadata) and the leadership plus cursor
// (etcd.CampaignMetaRebuild). Neither is useful alone.
//
// Returning is not failure: a node that has nothing to do, or that lost the
// election to a node which then finished the job, returns nil.
func (rt *clusterRuntime) RunMetaRebuild(ctx context.Context) error {
	if rt.index == nil || rt.index.Scope() != metastore.ScopeCluster {
		// Local scope rebuilds per node. Nothing cluster-wide to run.
		return nil
	}

	needed, err := rt.metaRebuildNeeded(ctx)
	if err != nil || !needed {
		return err
	}

	rt.lg.Info("Campaigning for cluster metadata rebuild leadership",
		zap.String("candidate", rt.rebuildCandidate()))

	lead, err := etcd.CampaignMetaRebuild(ctx, rt.client, rt.etcdCfg, rt.rebuildCandidate())
	if err != nil {
		return errors.Wrap(err, "campaign")
	}

	defer func() { _ = lead.Close() }()

	// Losing the lease must stop the walk: a standby resumes from the last
	// checkpoint, and two runners writing under one cursor would move it
	// backwards.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-lead.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	// Asked again, now that we hold it. Campaigning blocks, and what a node
	// waits behind is another node doing this exact job — so by the time the
	// election returns, the work is often already done. Without this check
	// every node in the cluster rebuilds the whole cluster in turn, each one
	// emptying what the last one built.
	needed, err = rt.metaRebuildNeeded(ctx)
	if err != nil || !needed {
		rt.lg.Debug("Cluster metadata rebuild already completed by another node")

		return err
	}

	return rt.rebuildMetadata(ctx, lead)
}

// metaRebuildNeeded reports whether the store wants building.
//
// Two independent reasons, and the cursor is the one that is easy to miss: a
// store can read ready while a rebuild is half-finished, if a previous runner
// died after marking it. The cursor is what says a walk is in flight, and it
// outranks the state.
func (rt *clusterRuntime) metaRebuildNeeded(ctx context.Context) (bool, error) {
	if _, ok, err := etcd.LoadMetaRebuildCursor(ctx, rt.client, rt.etcdCfg); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}

	state, err := rt.index.State(ctx)
	if err != nil {
		return false, errors.Wrap(err, "read metadata store state")
	}

	return state != metastore.StateReady, nil
}

// rebuildMetadata runs the elected walk: resume from the cursor, checkpoint per
// batch, clear the cursor once the store is ready.
func (rt *clusterRuntime) rebuildMetadata(ctx context.Context, lead *etcd.MetaRebuildLeadership) error {
	var (
		resume   clusterstore.RebuildCursor
		resuming bool
	)

	raw, ok, err := etcd.LoadMetaRebuildCursor(ctx, rt.client, rt.etcdCfg)
	if err != nil {
		return err
	}

	if ok {
		if resume, err = clusterstore.DecodeRebuildCursor(raw); err != nil {
			return err
		}

		resuming = true
	}

	rt.lg.Info("Cluster metadata rebuild elected and running",
		zap.String("resume_bucket", resume.Bucket),
		zap.String("resume_key", resume.Key),
		zap.Bool("resuming", resuming),
	)

	report, err := rt.coord.RebuildMetadata(ctx, rt.index, clusterstore.RebuildOptions{
		Resume:   resume,
		Resuming: resuming,
		Checkpoint: func(ctx context.Context, cur clusterstore.RebuildCursor) error {
			encoded, err := cur.Encode()
			if err != nil {
				return err
			}

			return lead.SaveCursor(ctx, encoded)
		},
	})
	if err != nil {
		return err
	}

	// Cleared last, and only now. An absent cursor means "no rebuild in
	// progress", so clearing before the store was marked ready would let the
	// next runner start over a walk that had in fact finished.
	//
	// Detached from ctx: the walk is done and the cursor must not outlive it
	// just because the process is shutting down — a stale cursor costs the next
	// node a full rebuild of a store that is already correct.
	if err := lead.ClearCursor(context.WithoutCancel(ctx)); err != nil {
		rt.lg.Warn("Clearing metadata rebuild cursor failed; the next runner will redo the walk",
			zap.Error(err))
	}

	rt.lg.Info("Cluster metadata rebuild done",
		zap.Int("buckets", report.Buckets),
		zap.Int("objects", report.Objects),
	)

	return nil
}

// rebuildCandidate labels this node in the election. It is diagnostic — the
// election is decided by etcd, not by the label — but it is what an operator
// reads to find which node is holding the rebuild.
func (rt *clusterRuntime) rebuildCandidate() string {
	return string(rt.nodeID) + "/metarebuild"
}
