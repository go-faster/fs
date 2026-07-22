package clusterstore

import (
	"context"
	"io"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// Get opens an object for reading: the sidecar is fetched from the first
// reachable placement target, then the payload streams from the first
// available replica (open-time failover) or is gathered from any k EC shards,
// reconstructing missing ones in memory. It returns ErrNotFound when no
// target holds a committed sidecar and ErrUnrecoverable when too many
// fragments are gone.
func (c *Coordinator) Get(ctx context.Context, bucket, key string) (*Sidecar, io.ReadCloser, error) {
	topo := c.topo.Topology()

	sc, err := c.fetchSidecar(ctx, topo, bucket, key)
	if err != nil {
		return nil, nil, err
	}

	s, plan, peers, err := c.planFor(topo, sc)
	if err != nil {
		return nil, nil, err
	}

	if s.Kind != scheme.EC {
		rc, err := c.openReplica(ctx, plan, peers, sc, s)
		if err != nil {
			return nil, nil, err
		}

		return sc, rc, nil
	}

	return sc, c.openEC(ctx, plan, peers, sc, s), nil
}

// Stat returns an object's sidecar without touching payload fragments.
func (c *Coordinator) Stat(ctx context.Context, bucket, key string) (*Sidecar, error) {
	return c.fetchSidecar(ctx, c.topo.Topology(), bucket, key)
}

// Delete removes an object: its sidecars first (the commit records — once
// they are gone the object is unreadable everywhere), then the generation's
// fragments, best-effort across all placement targets. Deleting an absent
// object returns ErrNotFound.
func (c *Coordinator) Delete(ctx context.Context, bucket, key string) error {
	c.waitKey(bucket, key)

	topo := c.topo.Topology()

	sc, err := c.fetchSidecar(ctx, topo, bucket, key)
	if err != nil {
		return err
	}

	_, plan, peers, err := c.planFor(topo, sc)
	if err != nil {
		return err
	}

	name := sidecarName(bucket, key)

	var firstErr error

	for i := range plan {
		if err := peers[i].Delete(ctx, plan[i].Target.Disk, name); err != nil && !errors.Is(err, transport.ErrNotFound) {
			if firstErr == nil {
				firstErr = errors.Wrapf(err, "delete sidecar on %s/%s", plan[i].Target.Node, plan[i].Target.Disk)
			}

			continue
		}

		frag := fragmentName(bucket, key, sc.Generation, plan[i].Index)
		if err := peers[i].Delete(ctx, plan[i].Target.Disk, frag); err != nil && !errors.Is(err, transport.ErrNotFound) {
			if firstErr == nil {
				firstErr = errors.Wrapf(err, "delete fragment on %s/%s", plan[i].Target.Node, plan[i].Target.Disk)
			}
		}
	}

	return firstErr
}

// planFor recomputes an object's fragment plan from its sidecar (the recorded
// scheme and size) and dials the involved peers.
func (c *Coordinator) planFor(topo *cluster.Topology, sc *Sidecar) (scheme.Scheme, []fragment.Item, []Peer, error) {
	s, err := sc.ParseScheme()
	if err != nil {
		return scheme.Scheme{}, nil, nil, err
	}

	plan, err := fragment.Plan(topo, s, placement.ObjectKey(sc.Bucket, sc.Key), sc.Size)
	if err != nil {
		return scheme.Scheme{}, nil, nil, err
	}

	peers, err := c.dialPlan(topo, plan)
	if err != nil {
		return scheme.Scheme{}, nil, nil, err
	}

	return s, plan, peers, nil
}

// openReplica streams the first available full replica, failing over across
// replica targets at open time.
func (c *Coordinator) openReplica(ctx context.Context, plan []fragment.Item, peers []Peer, sc *Sidecar, s scheme.Scheme) (io.ReadCloser, error) {
	var lastErr error

	for i := range s.FullReplicas() {
		name := fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[i].Index)

		rc, size, err := peers[i].Get(ctx, plan[i].Target.Disk, name)
		if err != nil {
			lastErr = err
			continue
		}

		if size != sc.Size {
			// A torn or stale fragment; never serve it.
			_ = rc.Close()
			lastErr = errors.Errorf("replica %d has size %d, want %d", i, size, sc.Size)

			continue
		}

		return rc, nil
	}

	return nil, errors.Wrapf(ErrUnrecoverable, "no readable replica (last error: %v)", lastErr)
}

// openEC streams an EC object through a pipe: data shards are joined, missing
// ones reconstructed from parity into in-memory staging. Decode errors
// (including ErrUnrecoverable) surface through the returned reader.
func (c *Coordinator) openEC(ctx context.Context, plan []fragment.Item, peers []Peer, sc *Sidecar, s scheme.Scheme) io.ReadCloser {
	open := func(index int) (io.ReadCloser, error) { //nolint:unparam // OpenFunc contract: (nil, nil) marks a lost shard.
		name := fragmentName(sc.Bucket, sc.Key, sc.Generation, index)

		rc, size, err := peers[index].Get(ctx, plan[index].Target.Disk, name)
		if err != nil {
			// Lost shard: reconstruction decides whether enough survive.
			return nil, nil //nolint:nilnil // (nil, nil) is the OpenFunc "lost" contract.
		}

		if size != plan[index].Size {
			_ = rc.Close()
			return nil, nil //nolint:nilnil // Torn shard counts as lost.
		}

		return rc, nil
	}

	pr, pw := io.Pipe()

	go func() {
		err := fragment.DecodeStream(s, sc.Size, open, memStage, pw)
		pw.CloseWithError(err)
	}()

	return pr
}

// fetchSidecar reads the object's commit record from the first reachable
// placement target. Candidate targets cover the bucket's configured scheme
// and the replica-scheme layout, so objects written before a bucket scheme
// change stay reachable. All-absent means ErrNotFound; when every candidate
// errored otherwise, that error surfaces (an unreachable cluster must not
// read as "object gone").
func (c *Coordinator) fetchSidecar(ctx context.Context, topo *cluster.Topology, bucket, key string) (*Sidecar, error) {
	name := sidecarName(bucket, key)

	var lastErr error

	for _, t := range c.sidecarCandidates(topo, bucket, key) {
		p, err := c.dial(topo, t.Node)
		if err != nil {
			lastErr = err
			continue
		}

		rc, _, err := p.Get(ctx, t.Disk, name)
		if err != nil {
			if !errors.Is(err, transport.ErrNotFound) {
				lastErr = err
			}

			continue
		}

		data, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			lastErr = errors.Wrap(err, "read sidecar")
			continue
		}

		sc, err := decodeSidecar(data)
		if err != nil {
			lastErr = err
			continue
		}

		return sc, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, errors.Wrapf(ErrNotFound, "%s/%s", bucket, key)
}

// sidecarCandidates lists the targets that may hold the object's sidecar, in
// placement order: the bucket's configured scheme first, then the 3-target
// replica layout and the default EC layout for objects written under an
// earlier bucket scheme.
func (c *Coordinator) sidecarCandidates(topo *cluster.Topology, bucket, key string) []placement.Target {
	pkey := placement.ObjectKey(bucket, key)
	counts := []int{c.schemeFor(bucket).Copies(), scheme.Default.Copies(), scheme.DefaultEC.Copies()}

	seen := make(map[placement.Target]struct{})

	var out []placement.Target

	for _, n := range counts {
		for _, t := range placement.Place(topo, pkey, n) {
			if _, ok := seen[t]; ok {
				continue
			}

			seen[t] = struct{}{}

			out = append(out, t)
		}
	}

	return out
}
