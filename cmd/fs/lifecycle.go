package main

import (
	"context"

	"go.uber.org/zap"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
	"github.com/go-faster/fs/internal/lastrun"
	"github.com/go-faster/fs/internal/lifecycle"
)

// runLifecycle enforces bucket lifecycle rules on a single node until ctx is
// canceled.
//
// A disabled sweep is reported at warning level rather than passed over in
// silence. The ?lifecycle subresource still accepts rules — it is the same
// bucket metadata either way — so an operator who turns enforcement off has a
// server that stores expiry rules and deletes nothing, and that has to be
// visible in the log rather than discovered from objects that never went away.
func runLifecycle(
	ctx context.Context,
	lg *zap.Logger,
	storage fs.Storage,
	cfg LifecycleConfig,
	state lastrun.Store,
	member *peer.Member,
) {
	if cfg.Interval <= 0 {
		lg.Warn("Lifecycle enforcement is disabled; rules clients set will be stored but never applied")
		return
	}

	sweeper := &lifecycle.Sweeper{Storage: storage, Log: lg, State: state}
	if member != nil {
		sweeper.Owns = func(bucket string) bool { return sweepsBucket(member, bucket) }
	}

	sweeper.Run(ctx, cfg.Interval)
}

// sweepsBucket elects one lifecycle sweeper per bucket in a cluster: the
// first node of the partition the bucket's name maps to that gossip last
// reached. Buckets spread over the nodes, and a node that is down hands its
// buckets to the next. Two nodes that see liveness differently for a moment
// may both sweep a bucket; its deletes are conditional, so the cost is a
// duplicated listing.
func sweepsBucket(member *peer.Member, bucket string) bool {
	l := member.Layout()
	if l == nil {
		return false
	}

	up := map[layout.NodeID]bool{member.ID(): true}

	for _, p := range member.Peers() {
		if p.Err == nil && !p.Seen.IsZero() {
			up[p.ID] = true
		}
	}

	for _, id := range l.Slots[l.Partition([]byte(bucket))] {
		if up[id] {
			return id == member.ID()
		}
	}

	return false
}
