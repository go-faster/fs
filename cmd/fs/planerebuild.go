package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/cluster/metastore"
)

// planeRebuildInterval is how often a node checks whether the plane owes a
// rebuild.
//
// Slow, and it can afford to be. A rebuild is a walk of every disk in the
// cluster; the difference between noticing one is owed now and noticing it in
// half a minute is nothing against the hours that follow. What the interval
// bounds is how long a plane stays degraded after a failure before anything
// starts putting it right, and half a minute is well inside the time a failover
// itself takes to settle.
const planeRebuildInterval = 30 * time.Second

// RunPlaneRebuild starts the cluster-wide rebuild the plane owes, when the
// reason it owes one is a reason to act without being asked.
//
// # The piece that was missing
//
// The walk, the election and the resume cursor have all existed since E3, and
// nothing called them: a plane that lost a range said so in its logs and in
// fs.cluster.plane.ready, and then stayed degraded until an operator noticed.
// Listings were correct meanwhile — they fall back to the sidecar walk — so the
// failure mode was a cluster that was quietly slow forever, which is the kind
// that goes unnoticed longest.
//
// # Why it is not simply automatic
//
// Because the two ways a plane ends up unbuilt are not the same event.
//
// Switching the plane on over a cluster that already holds objects is planned.
// An operator chose the moment; they can choose the window for the walk, which
// on a cluster this project targets is hours of I/O competing with serving
// traffic. Starting that the instant the config lands would take the decision
// away from them and pick the worst possible moment — the one right after a
// change.
//
// A failure that leaves a range with no copy of its data is not planned, and
// nothing about waiting improves it. The plane is degraded now, every listing
// in the cluster is on the slow path, and the rebuild is the only thing that
// ends it.
//
// So the default acts on the second and waits on the first, and the cause
// recorded on the flag is what tells them apart. A plane building for a reason
// nobody recorded is left alone: a rebuild is too expensive to start on a guess.
func (rt *clusterRuntime) RunPlaneRebuild(ctx context.Context, cfg Config) {
	lg := rt.lg

	ticker := time.NewTicker(planeRebuildInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		build, err := rt.metaPlane.state.Status(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			lg.Warn("Reading the metadata plane flag failed", zap.Error(err))

			continue
		}

		if build.State == metastore.StateReady {
			continue
		}

		if !rebuildWanted(cfg.MetadataRebuild(), build.Cause) {
			continue
		}

		lg.Info("Metadata plane owes a rebuild: starting one",
			zap.String("cause", build.Cause.String()),
			zap.String("policy", cfg.MetadataRebuild()))

		// Elected, resumable and idempotent, all of which RunMetaRebuild
		// already is — including the recheck after winning the election, which
		// is what stops every node rebuilding the cluster in turn. So a tick
		// that races another node's rebuild costs one campaign and no work.
		if err := rt.RunMetaRebuild(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}

			// Retried on the next tick. Every reason this fails — a lost
			// election, a control-plane read, a node going away mid-walk — is
			// one the next attempt may not have, and the cursor means it
			// resumes rather than starts over.
			lg.Warn("Metadata plane rebuild failed; retrying", zap.Error(err))
		}
	}
}

// rebuildWanted reports whether a plane building for this reason should be
// rebuilt without an operator saying so.
//
// An unspecified cause is never acted on, whatever the policy — including
// "always". It is what a plane reads as when its flag cannot be read, when an
// older cluster wrote it, or when someone started a rebuild by hand, and none
// of those is a statement that a fresh walk of every disk is wanted now.
func rebuildWanted(policy string, cause metastore.BuildCause) bool {
	switch cause {
	case metastore.CauseOrphaned:
		return policy != RebuildNever
	case metastore.CauseNeverBuilt:
		return policy == RebuildAlways
	case metastore.CauseUnspecified:
		return false
	default:
		return false
	}
}
