package main

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// metadataPlane is this node's participation in the sharded metadata plane,
// with everything that has to be closed when the node stops.
type metadataPlane struct {
	plane *shardstore.Plane
	load  shardstore.Loader
	shard *shardstore.Shard
	state *etcd.PlaneState
}

// Store is the cluster-scope metastore this node serves.
func (p *metadataPlane) Store() *shardstore.Store { return p.plane.Store() }

// planeDeps is what building the plane needs from an already-wired node.
type planeDeps struct {
	self    cluster.NodeID
	root    string
	client  *clientv3.Client
	etcdCfg etcd.Config
	// topology resolves a node ID to its registered address, which is how a
	// range's owner becomes something to dial.
	topology func() *cluster.Topology
	// peers is the node's existing connection cache, reused rather than
	// duplicated: the plane talks to the same nodes over the same transport.
	peers *clusterstore.HTTPPeers
}

// openMetadataPlane opens this node's shard and wires it to routing, the
// cluster-scope store, and the plane's readiness flag.
//
// It does not partition the plane and does not fail over — bootstrapping and
// reconciliation are elected, cluster-wide decisions, and a node making them on
// its own way up would have every node making them at once. A node that starts
// against an unpartitioned plane simply routes nothing until one exists.
func openMetadataPlane(ctx context.Context, deps planeDeps) (*metadataPlane, error) {
	shard, err := shardstore.OpenShard(shardstore.DefaultShardDir(deps.root))
	if err != nil {
		return nil, errors.Wrap(err, "open metadata shard")
	}

	state, err := etcd.NewPlaneState(ctx, deps.client, deps.etcdCfg)
	if err != nil {
		_ = shard.Close()

		return nil, errors.Wrap(err, "metadata plane state")
	}

	load := func(ctx context.Context) (*rangemap.Map, error) {
		m, ok, err := etcd.LoadRangeMap(ctx, deps.client, deps.etcdCfg)
		if err != nil {
			return nil, err
		}

		if !ok {
			// Not an error: a cluster whose plane has not been partitioned yet
			// is a state, and routing reports it as one. An error here would
			// have every request on a fresh cluster fail rather than fall back.
			return &rangemap.Map{}, nil
		}

		return m, nil
	}

	dial := func(_ context.Context, node cluster.NodeID) (shardstore.Backend, error) {
		topo := deps.topology()
		if topo == nil {
			return nil, errors.New("no topology to resolve a range owner")
		}

		for i := range topo.Nodes {
			if topo.Nodes[i].ID != node {
				continue
			}

			client, err := deps.peers.Client(topo.Nodes[i])
			if err != nil {
				return nil, err
			}

			return shardstore.NewPeer(client), nil
		}

		// A range whose owner has left the topology. The request fails rather
		// than being answered from somewhere else, which is the whole point:
		// the owner is the only node that knows what it has applied.
		return nil, errors.Errorf("range owner %q is not in the topology", node)
	}

	return &metadataPlane{
		plane: shardstore.NewPlane(deps.self, shard, load, dial, state),
		load:  load,
		shard: shard,
		state: state,
	}, nil
}

// Close releases the shard and stops watching the readiness flag.
//
// The watch first: it is the only part with a goroutine, and a shard closed
// underneath one would be a use-after-close waiting for the right timing.
func (p *metadataPlane) Close() error {
	var err error

	if cerr := p.state.Close(); cerr != nil {
		err = cerr
	}

	if cerr := p.shard.Close(); cerr != nil && err == nil {
		err = cerr
	}

	return err
}

// peerServerOptions adds the shard endpoint when this node runs a plane.
//
// Only then: a node without a shard answering shard requests would report
// "not owned" for keys it was never given, which reads to the caller as a stale
// map rather than as a node that does not run the plane at all.
func peerServerOptions(rt *clusterRuntime) []transport.ServerOption {
	if rt.metaPlane == nil {
		return nil
	}

	return []transport.ServerOption{transport.WithShard(shardstore.Serve(rt.metaPlane.shard))}
}

// planeReconcileInterval is how often the elected controller checks membership
// against the partitioning.
//
// A pass on a healthy cluster reads the map and writes nothing, so this is
// cheap to run often — and the grace periods, not this, are what decide how
// fast a failure is acted on. Too long a tick would add itself to those graces;
// too short would read etcd for no reason.
const planeReconcileInterval = 5 * time.Second

// RunPlaneController holds the cluster-wide plane election and reconciles the
// partitioning with the membership until ctx is done.
//
// Elected because the partitioning has one writer. Every node runs this and all
// but one block in the campaign, so a controller dying is replaced by whichever
// node the election hands it to next, with no operator action and no node
// designated in advance.
func (rt *clusterRuntime) RunPlaneController(ctx context.Context, cfg Config) {
	lg := rt.lg

	for {
		lead, err := etcd.CampaignPlane(ctx, rt.client, rt.etcdCfg, string(rt.nodeID)+"/plane")
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			// A failed campaign is a control-plane problem, not a reason to
			// stop being a candidate: a cluster whose nodes all gave up on the
			// election would never fail over again.
			lg.Warn("Metadata plane election failed, retrying", zap.Error(err))

			if !sleepCtx(ctx, planeReconcileInterval) {
				return
			}

			continue
		}

		lg.Info("Holding metadata plane leadership")

		err = servePlaneLeadership(ctx, lg, rt, cfg, lead)

		_ = lead.Close()

		if ctx.Err() != nil {
			return
		}

		if err != nil {
			lg.Warn("Metadata plane leadership ended", zap.Error(err))
		}
	}
}

// servePlaneLeadership bootstraps the partitioning if there is none, then
// reconciles until leadership is lost.
func servePlaneLeadership(
	ctx context.Context,
	lg *zap.Logger,
	rt *clusterRuntime,
	cfg Config,
	lead *etcd.PlaneLeadership,
) error {
	// Bound to leadership: a controller that lost its lease mid-pass must stop
	// writing, not finish the write it was making.
	leadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-leadCtx.Done():
		case <-lead.Done():
			cancel()
		}
	}()

	if err := bootstrapPlane(leadCtx, lg, rt, cfg, lead); err != nil {
		return err
	}

	ctl, err := shardstore.NewController(shardstore.ControllerConfig{
		Load: rt.metaPlane.loadMap,
		Save: func(ctx context.Context, m *rangemap.Map) error {
			if err := lead.Check(ctx); err != nil {
				return err
			}

			return etcd.SaveRangeMap(ctx, rt.client, rt.etcdCfg, m)
		},
		Live:      rt.liveNodes,
		Readiness: rt.metaPlane.state,
		Measure:   rt.metaPlane.plane.Measure,
		Ready:     rt.metaPlane.plane.Ready,
		Split: shardstore.SplitPolicy{
			MaxBytes:         cfg.MetadataMaxRangeBytes(),
			MaxSplitsPerPass: cfg.MetadataMaxSplitsPerPass(),
		},
	})
	if err != nil {
		return err
	}

	ticker := time.NewTicker(planeReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-leadCtx.Done():
			return nil
		case <-ticker.C:
		}

		out, err := ctl.Reconcile(leadCtx)
		if err != nil {
			if leadCtx.Err() != nil {
				return nil
			}

			// Reported and retried. Every reason a pass refuses — a
			// control-plane read that failed, a membership too broken to act on
			// — is one that the next pass may not have.
			lg.Warn("Metadata plane reconciliation failed", zap.Error(err))

			continue
		}

		if !out.Changed {
			continue
		}

		if len(out.Learned) > 0 {
			lg.Info("Metadata ranges finished being copied: learners promoted to followers",
				zap.Int("ranges", len(out.Learned)))
		}

		if len(out.Split) > 0 {
			lg.Info("Metadata plane split",
				zap.Int("boundaries", len(out.Split)),
				zap.String("first", out.Split[0]))
		}

		if len(out.Promoted)+len(out.Orphaned)+len(out.Held) > 0 {
			lg.Info("Metadata plane reassigned",
				zap.Int("promoted", len(out.Promoted)),
				zap.Int("orphaned", len(out.Orphaned)),
				zap.Int("held", len(out.Held)))
		}

		if out.RebuildOwed() {
			// Said loudly. The plane is marked building, so listings are
			// already correct-and-slow, but nothing here starts the rebuild —
			// that is the elected rebuild runner's job and an operator's
			// decision about when to pay for it.
			lg.Warn("Metadata plane has ranges that hold nothing: a rebuild is owed",
				zap.Int("orphaned", len(out.Orphaned)))
		}
	}
}

// planeCatchUpInterval is how often a node checks whether it is being copied
// into, and how often a copy in progress makes another attempt.
//
// Slower than the controller's tick: a move is minutes of work at best, so
// checking three times a minute adds nothing to how fast one finishes. What it
// bounds is how long a node takes to *notice* it has become a learner, which is
// dead time at the start of every move — so not much slower either.
const planeCatchUpInterval = 15 * time.Second

// RunPlaneCatchUp copies the ranges this node is being moved, until ctx is done.
//
// Unelected, unlike the controller, and the difference is the point. Deciding
// that a range moves is a decision about the map, which has one writer. Copying
// it is work only the destination can do, so every node runs its own pass — and
// a node that is a learner of nothing does nothing, which is almost every node
// almost always.
func (rt *clusterRuntime) RunPlaneCatchUp(ctx context.Context) {
	lg := rt.lg

	ticker := time.NewTicker(planeCatchUpInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		out, err := rt.metaPlane.plane.CatchUp(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			// Retried rather than escalated: the only way a pass fails outright
			// is a control-plane read, which the next pass may well get.
			lg.Warn("Metadata plane catch-up failed", zap.Error(err))

			continue
		}

		if len(out.Copied) > 0 {
			lg.Info("Metadata ranges copied into this node",
				zap.Int("ranges", len(out.Copied)),
				zap.Int("entries", out.Entries),
				zap.Int("ready", len(out.Ready)))
		}

		if len(out.Failed) > 0 {
			// Said out loud. A move that cannot reach its source is stalled, and
			// a stalled move is invisible otherwise — the range keeps being
			// served by its owner, so nothing degrades and nothing completes.
			lg.Warn("Metadata ranges could not be copied into this node",
				zap.Int("ranges", len(out.Failed)),
				zap.Int("learning", out.Learning))
		}
	}
}

// bootstrapPlane partitions the plane if it has never been partitioned.
//
// Under leadership, so a cluster starting all its nodes at once partitions once
// rather than N times. Idempotent anyway — Initialize leaves a live
// partitioning alone — because the election is what makes it one writer, and
// "there is one writer" is a thing to rely on rather than a thing to assume.
func bootstrapPlane(
	ctx context.Context,
	lg *zap.Logger,
	rt *clusterRuntime,
	cfg Config,
	lead *etcd.PlaneLeadership,
) error {
	topo := rt.coord.Topology()
	if topo == nil || len(topo.Nodes) == 0 {
		return errors.New("no topology to partition the plane over")
	}

	created, err := shardstore.Initialize(ctx,
		rt.metaPlane.loadMap,
		func(ctx context.Context, m *rangemap.Map) error {
			if err := lead.Check(ctx); err != nil {
				return err
			}

			return etcd.SaveRangeMap(ctx, rt.client, rt.etcdCfg, m)
		},
		cfg.MetadataRanges(), topo.Nodes, cfg.MetadataReplicas())
	if err != nil {
		return errors.Wrap(err, "partition the metadata plane")
	}

	if created {
		lg.Info("Metadata plane partitioned",
			zap.Int("ranges", cfg.MetadataRanges()),
			zap.Int("replicas", cfg.MetadataReplicas()),
			zap.Int("nodes", len(topo.Nodes)))
	}

	return nil
}

// loadMap reads the partitioning, reporting an unpartitioned plane as an empty
// map rather than an error — the same distinction routing makes.
func (p *metadataPlane) loadMap(ctx context.Context) (*rangemap.Map, error) {
	return p.load(ctx)
}

// liveNodes is the membership the controller reconciles against: whoever is
// registered right now.
func (rt *clusterRuntime) liveNodes() []cluster.NodeID {
	topo := rt.coord.Topology()
	if topo == nil {
		return nil
	}

	out := make([]cluster.NodeID, 0, len(topo.Nodes))
	for i := range topo.Nodes {
		out = append(out, topo.Nodes[i].ID)
	}

	return out
}
