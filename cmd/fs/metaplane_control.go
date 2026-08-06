package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// planeController is the sharded metadata plane behind the admin API, and the
// one guard both ways of starting a rebuild go through.
//
// Two things start rebuilds: the timed policy, which acts when a failure left a
// range with no copy of its data, and an operator, who acts when they have
// decided the moment. They must not run at once on one node. Both would
// campaign, so the second would block on the election until the first finished
// — safe, and a whole rebuild's worth of a goroutine waiting to do nothing.
type planeController struct {
	lg *zap.Logger
	// run is the elected, cursor-checkpointed walk. Injected rather than
	// called through the runtime so a test can drive this without a cluster.
	run func(context.Context) error
	// status reads the plane's flag and why it is set.
	status func(context.Context) (metastore.Build, error)
	// policy is the configured automatic-rebuild policy, reported so an
	// operator can see why nothing has happened on its own.
	policy string
	// loadMap reads the partitioning from the control plane, which is the
	// authoritative one: every node's own copy is a cache that lazy routing is
	// allowed to leave behind.
	loadMap func(context.Context) (*rangemap.Map, error)
	// topo is the registered membership, which is what makes a range "held":
	// an owner that is not in it is an owner nothing can reach.
	topo clusterstore.TopologySource
	// live asks the nodes themselves which revision they are routing by. Nil
	// leaves the per-node view empty rather than wrong.
	live *peerStatus
	// baseCtx bounds every run: the server's lifetime, not the API request's.
	//
	// A rebuild outlives the request that asked for it by hours. Run on the
	// request's context it would be canceled the moment the response was
	// written, and an operator would see a rebuild start and silently stop.
	baseCtx context.Context

	mu                    sync.Mutex
	running               bool
	startedAt, finishedAt time.Time
	lastErr               string
}

var _ adminhandler.PlaneControl = (*planeController)(nil)

// Status implements adminhandler.PlaneControl.
func (c *planeController) Status(ctx context.Context) (adminhandler.PlaneStatus, error) {
	build, err := c.status(ctx)
	if err != nil {
		return adminhandler.PlaneStatus{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	out := adminhandler.PlaneStatus{
		Ready:      build.State == metastore.StateReady,
		Rebuilding: c.running,
		Policy:     c.policy,
		StartedAt:  c.startedAt,
		FinishedAt: c.finishedAt,
		Err:        c.lastErr,
	}

	if !out.Ready {
		out.Cause = build.Cause.String()
	}

	c.describe(ctx, &out)

	return out, nil
}

// describe fills in the partitioning and what each node believes about it.
//
// Best effort, and deliberately: the flag and the rebuild state above are what
// the endpoint owes, and a control plane that cannot be read must not turn the
// whole answer into an error. An operator looking at a plane during an etcd
// outage still wants to know whether this node is rebuilding.
func (c *planeController) describe(ctx context.Context, out *adminhandler.PlaneStatus) {
	if c.loadMap == nil {
		return
	}

	m, err := c.loadMap(ctx)
	if err != nil || m == nil {
		return
	}

	out.Revision = m.Revision

	var registered []cluster.Node

	if c.topo != nil {
		if topo := c.topo.Topology(); topo != nil {
			registered = topo.Nodes
		}
	}

	out.Ranges = planeRanges(m, registered)
	out.Nodes = c.planeNodes(ctx, m, registered)
}

// planeRanges renders the partitioning, marking the ranges nobody is serving.
//
// A range is held when its owner is not registered. That is the one plane
// question with no other signal: the metrics are per node, and from a node's
// own shard a range owned by a node that is gone looks exactly like a range
// owned by a node that is fine.
func planeRanges(m *rangemap.Map, registered []cluster.Node) []adminhandler.PlaneRange {
	live := make(map[cluster.NodeID]bool, len(registered))
	for _, n := range registered {
		live[n.ID] = true
	}

	out := make([]adminhandler.PlaneRange, 0, len(m.Ranges))

	for _, r := range m.Ranges {
		out = append(out, adminhandler.PlaneRange{
			Start:     r.Start,
			End:       r.End,
			Owner:     string(r.Owner),
			Followers: nodeIDs(r.Followers),
			Learners:  nodeIDs(r.Learners),
			MoveTo:    string(r.MoveTo),
			// Registered nodes only. A map naming an owner the registry has
			// never heard of is held for the same reason one naming a departed
			// node is: nothing can reach it.
			Held: !live[r.Owner],
		})
	}

	return out
}

// planeNodes asks each node which revision it is routing by.
//
// The disagreement is the point. Routing is lazy — a node refreshes when a peer
// says it is behind, and not otherwise — so a node taking no traffic for a
// range that moved can sit on a stale map indefinitely, and nothing else would
// say so.
func (c *planeController) planeNodes(
	ctx context.Context,
	m *rangemap.Map,
	registered []cluster.Node,
) []adminhandler.PlaneNode {
	if c.live == nil {
		return nil
	}

	return planeNodeRows(m, registered, c.live.Fetch(ctx, registered))
}

// planeNodeRows renders what each node said about the map against what the map
// actually is.
//
// Separated from the fetching so the comparison — the part with a rule in it —
// can be tested without a cluster to ask.
func planeNodeRows(
	m *rangemap.Map,
	registered []cluster.Node,
	fetched map[cluster.NodeID]nodeLiveResult,
) []adminhandler.PlaneNode {
	out := make([]adminhandler.PlaneNode, 0, len(registered))

	for _, n := range registered {
		got, ok := fetched[n.ID]

		node := adminhandler.PlaneNode{ID: string(n.ID), Live: true, Reporting: ok && got.Live != nil}
		if !node.Reporting {
			// Nothing is inferred from silence. A node that did not answer is
			// not a node routing by revision zero, and reporting it as behind
			// would put every unreachable node on the list of stale ones.
			out = append(out, node)

			continue
		}

		node.Revision = got.Live.PlaneRevision
		node.Owned = got.Live.PlaneOwned
		node.Replicated = got.Live.PlaneReplicated
		// A node with no map at all is behind by definition: it is routing by
		// nothing. Reported the same way as one behind by a revision, because
		// the operator's next move is the same.
		node.Behind = node.Revision < m.Revision

		out = append(out, node)
	}

	return out
}

// nodeIDs renders a node list for the wire.
func nodeIDs(in []cluster.NodeID) []string {
	if len(in) == 0 {
		return nil
	}

	out := make([]string, 0, len(in))
	for _, id := range in {
		out = append(out, string(id))
	}

	return out
}

// Rebuild implements adminhandler.PlaneControl: start now, whatever the policy.
//
// Returns as soon as the walk is launched. It is hours on a cluster of any
// size, so a request that waited for it would time out long before it finished
// and leave an operator unable to tell a rebuild that was running from one that
// never started.
func (c *planeController) Rebuild(context.Context) error {
	return c.start(func() {
		c.lg.Info("Metadata plane rebuild requested by an operator",
			zap.String("policy", c.policy))
	})
}

// start launches a rebuild unless this node is already running one.
//
// The whole point of the type: the timer and the operator both come through
// here, so "already running" is one fact rather than two that have to agree.
func (c *planeController) start(announce func()) error {
	c.mu.Lock()

	if c.running {
		c.mu.Unlock()

		return adminhandler.ErrPlaneRebuildConflict
	}

	c.running = true
	c.startedAt = time.Now()
	c.finishedAt = time.Time{}
	c.lastErr = ""

	c.mu.Unlock()

	if announce != nil {
		announce()
	}

	go c.walk()

	return nil
}

// walk runs the rebuild and records what happened to it.
func (c *planeController) walk() {
	err := c.run(c.baseCtx)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.running = false
	c.finishedAt = time.Now()

	if err != nil {
		c.lastErr = err.Error()

		// Logged here rather than left to the caller: the caller is an HTTP
		// request that returned hours ago, or a ticker that has moved on.
		c.lg.Warn("Metadata plane rebuild failed", zap.Error(err))

		return
	}

	c.lg.Info("Metadata plane rebuild finished")
}

// Running reports whether this node is rebuilding, for the timed policy to
// avoid starting a second one.
func (c *planeController) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.running
}
