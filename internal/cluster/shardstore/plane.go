package shardstore

import (
	"context"
	"slices"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// Dialer opens a backend to another node.
//
// Only ever called for nodes other than this one — the local shard is reached
// directly, without a round trip through its own transport.
//
// Called on every routed request and on every shipped batch, so it is expected
// to hand back a cached connection rather than establish one. Dialing per write
// would put connection setup on the path of every object put.
type Dialer func(ctx context.Context, node cluster.NodeID) (Backend, error)

// Plane is one node's participation in the sharded metadata plane: its shard,
// its routing, and the cluster-scope store built on both.
//
// It exists because those three have to agree about the same map, and nothing
// else was making them. The shard serves ranges, the router routes to owners,
// and a node whose shard served ranges the router no longer routed to it would
// answer for keys nobody asks it about — and keep answering, with nothing to
// notice. So the map is loaded once, by the router, and the shard is
// reconfigured from that same load.
type Plane struct {
	self  cluster.NodeID
	shard *Shard

	router *Router
	store  *Store
	dial   Dialer
}

// NewPlane wires this node's shard, routing and store together.
//
// The shard is reconfigured on every map the router adopts, so a caller never
// applies a map by hand: routing a key is what keeps the node current.
func NewPlane(
	self cluster.NodeID,
	shard *Shard,
	load Loader,
	dial Dialer,
	ready Readiness,
) *Plane {
	p := &Plane{self: self, shard: shard, dial: dial}

	p.router = NewRouter(load)
	p.router.OnRefresh(p.apply)
	p.store = NewStore(p.router, p.resolve, ready)

	// The shard ships what it applies to its ranges' followers. Set here rather
	// than at OpenShard because shipping needs the dialer, which needs the map,
	// which the shard itself knows nothing about.
	shard.SetShipper(p.shipToFollowers)

	return p
}

// Store returns the cluster-scope metastore.Store this node serves.
func (p *Plane) Store() *Store { return p.store }

// Shard returns this node's shard.
func (p *Plane) Shard() *Shard { return p.shard }

// Measure asks a range's owner what it holds and where it would divide.
//
// Routed rather than answered locally, because the owner is the only node that
// holds the range — a controller measuring its own shard would be measuring
// whichever ranges it happens to own and calling that the cluster.
func (p *Plane) Measure(
	ctx context.Context,
	node cluster.NodeID,
	r rangemap.Range,
) (Measurement, error) {
	backend, err := p.resolve(ctx, node)
	if err != nil {
		return Measurement{}, err
	}

	return backend.Measure(ctx, r)
}

// Router returns this node's routing, for admin surfaces and metrics.
func (p *Plane) Router() *Router { return p.router }

// Refresh loads the map and reconfigures this node from it.
//
// Ordinary operation needs no call to this — routing a key refreshes when the
// cache is stale. It is for the paths that must act on a map change without
// waiting for traffic: a node starting up, and a controller that has just
// written a new partitioning.
func (p *Plane) Refresh(ctx context.Context) (*rangemap.Map, error) {
	return p.router.Reload(ctx)
}

// apply configures the shard from a map: serve what this node owns, replicate
// what it follows.
//
// Both, and separately. A range this node owns is served; a range it follows is
// held and not served, because a follower has no way to know it is current.
func (p *Plane) apply(m *rangemap.Map) {
	if m == nil {
		return
	}

	var followed []rangemap.Range

	for _, r := range m.Ranges {
		// Owning wins: a map that named this node both owner and follower of one
		// range would have it replaying its own writes from someone else.
		if r.Owner != p.self && slices.Contains(r.Followers, p.self) {
			followed = append(followed, r)
		}
	}

	p.shard.Configure(m.RangesFor(p.self), followed)
}

// resolve returns the backend serving a node: this node's own shard directly,
// anyone else's over the wire.
func (p *Plane) resolve(ctx context.Context, node cluster.NodeID) (Backend, error) {
	if node == p.self {
		return p.shard, nil
	}

	if p.dial == nil {
		return nil, errors.Errorf("no transport to reach node %s", node)
	}

	return p.dial(ctx, node)
}

// shipToFollowers sends a committed batch to each of the range's followers.
//
// Best effort, and serially: the Shipper contract requires batches for one
// range to arrive in order, and a failure costs a follower its currency — which
// costs a failover a rebuild — never an object, because the sidecars committed
// before this ran.
//
// A follower that cannot be reached is skipped rather than retried here.
// Retrying inside the write path would make an object write wait on a replica
// that exists to make failover cheap, which inverts what the two are for.
func (p *Plane) shipToFollowers(ctx context.Context, r rangemap.Range, repr []byte) {
	if p.dial == nil {
		return
	}

	for _, node := range r.Followers {
		if node == p.self {
			continue
		}

		backend, err := p.dial(ctx, node)
		if err != nil {
			continue
		}

		follower, ok := backend.(Replica)
		if !ok {
			continue
		}

		_ = follower.ApplyBatch(ctx, r, repr)
	}
}

// Replica is a backend that can replay an owner's batches. Satisfied by a Peer
// and by a local Shard.
type Replica interface {
	ApplyBatch(ctx context.Context, r rangemap.Range, repr []byte) error
}

var (
	_ Replica = (*Peer)(nil)
	_ Replica = (*Shard)(nil)
)

// Initialize writes a first partitioning if the plane has none.
//
// Idempotent: a plane that is already partitioned is left exactly as it is,
// because re-partitioning a live cluster would move every range at once. It
// reports whether it wrote one, so a caller can tell "a new cluster" from "one
// that was already running" — the first needs a rebuild before it can be
// trusted, the second does not.
func Initialize(
	ctx context.Context,
	load Loader,
	save func(context.Context, *rangemap.Map) error,
	ranges int,
	nodes []cluster.Node,
	replicas int,
) (created bool, err error) {
	// A load failure is not "there is no map". Treating it as one would have a
	// node that could not reach etcd repartition a live cluster from scratch,
	// which is the most expensive mistake available here.
	existing, err := load(ctx)
	if err != nil {
		return false, errors.Wrap(err, "read the current partitioning")
	}

	if existing != nil && len(existing.Ranges) > 0 {
		return false, nil
	}

	m, err := rangemap.Initial(ranges, nodes, replicas)
	if err != nil {
		return false, err
	}

	if err := save(ctx, m); err != nil {
		return false, err
	}

	return true, nil
}
