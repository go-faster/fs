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
// reconstructing missing ones in memory. Fragment lookups fall back across
// remembered topology epochs, so an object whose relocation has not finished
// is still served from its previous placement. It returns ErrNotFound when no
// target holds a committed sidecar and ErrUnrecoverable when too many
// fragments are gone.
func (c *Coordinator) Get(ctx context.Context, bucket, key string) (*Sidecar, io.ReadCloser, error) {
	sc, err := c.fetchSidecar(ctx, bucket, key)
	if err != nil {
		return nil, nil, err
	}

	s, err := sc.ParseScheme()
	if err != nil {
		return nil, nil, err
	}

	plans := c.epochPlans(s, sc.Bucket, sc.Key, sc.Size)
	if len(plans) == 0 {
		return nil, nil, errors.Wrap(ErrInsufficientTargets, "no epoch can place the scheme")
	}

	if s.Kind != scheme.EC {
		rc, err := c.openReplica(ctx, plans, sc, s)
		if err != nil {
			return nil, nil, err
		}

		return sc, rc, nil
	}

	return sc, c.openEC(ctx, plans, sc, s), nil
}

// Stat returns an object's sidecar without touching payload fragments.
func (c *Coordinator) Stat(ctx context.Context, bucket, key string) (*Sidecar, error) {
	return c.fetchSidecar(ctx, bucket, key)
}

// Delete removes an object: its sidecars first (the commit records — once
// they are gone the object is unreadable everywhere), then the generation's
// fragments, best-effort across the placement targets of every remembered
// epoch (a not-yet-relocated object still has state at the old placement).
// Deleting an absent object returns ErrNotFound.
func (c *Coordinator) Delete(ctx context.Context, bucket, key string) error {
	c.waitKey(bucket, key)

	sc, err := c.fetchSidecar(ctx, bucket, key)
	if err != nil {
		return err
	}

	s, err := sc.ParseScheme()
	if err != nil {
		return err
	}

	name := sidecarName(bucket, key)

	var firstErr error

	// NB: fragment deletes dedup by (disk, index) — under another epoch the
	// same disk can hold a different index of this object.
	type fragRef struct {
		ref diskRef
		idx int
	}

	fragSeen := make(map[fragRef]struct{})
	metaSeen := make(map[diskRef]struct{})

	for _, ep := range c.epochPlans(s, bucket, key, sc.Size) {
		for i := range ep.plan {
			p, err := c.dial(ep.topo, ep.plan[i].Target.Node)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}

				continue
			}

			disk := ep.plan[i].Target.Disk
			ref := targetRef(ep.plan[i].Target)

			if _, done := metaSeen[ref]; !done {
				metaSeen[ref] = struct{}{}

				if err := p.Delete(ctx, disk, name); err != nil && !errors.Is(err, transport.ErrNotFound) {
					if firstErr == nil {
						firstErr = errors.Wrapf(err, "delete sidecar on %s/%s", ep.plan[i].Target.Node, disk)
					}

					continue
				}
			}

			fr := fragRef{ref: ref, idx: ep.plan[i].Index}
			if _, done := fragSeen[fr]; done {
				continue
			}

			fragSeen[fr] = struct{}{}

			frag := fragmentName(bucket, key, sc.Generation, ep.plan[i].Index)
			if err := p.Delete(ctx, disk, frag); err != nil && !errors.Is(err, transport.ErrNotFound) {
				if firstErr == nil {
					firstErr = errors.Wrapf(err, "delete fragment on %s/%s", ep.plan[i].Target.Node, disk)
				}
			}
		}
	}

	return firstErr
}

// planFor recomputes an object's current-epoch fragment plan from its sidecar
// (the recorded scheme and size) and dials the involved peers.
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

// openFragment opens one fragment by index, trying each remembered epoch's
// target for it (newest first). Torn copies are skipped.
func (c *Coordinator) openFragment(ctx context.Context, plans []epochPlan, sc *Sidecar, index int) (io.ReadCloser, error) {
	name := fragmentName(sc.Bucket, sc.Key, sc.Generation, index)

	var lastErr error

	seen := make(map[diskRef]struct{})

	for _, ep := range plans {
		if index >= len(ep.plan) {
			continue
		}

		item := ep.plan[index]
		if _, done := seen[targetRef(item.Target)]; done {
			continue
		}

		seen[targetRef(item.Target)] = struct{}{}

		p, err := c.dial(ep.topo, item.Target.Node)
		if err != nil {
			lastErr = err
			continue
		}

		rc, size, err := p.Get(ctx, item.Target.Disk, name)
		if err != nil {
			lastErr = err
			continue
		}

		if size != item.Size {
			_ = rc.Close()
			lastErr = errors.Errorf("fragment %d has size %d, want %d", index, size, item.Size)

			continue
		}

		return rc, nil
	}

	if lastErr == nil {
		lastErr = transport.ErrNotFound
	}

	return nil, lastErr
}

// openReplica streams the first available full replica, failing over across
// replica indexes and topology epochs at open time.
func (c *Coordinator) openReplica(ctx context.Context, plans []epochPlan, sc *Sidecar, s scheme.Scheme) (io.ReadCloser, error) {
	var lastErr error

	for i := range s.FullReplicas() {
		rc, err := c.openFragment(ctx, plans, sc, i)
		if err != nil {
			lastErr = err
			continue
		}

		return rc, nil
	}

	return nil, errors.Wrapf(ErrUnrecoverable, "no readable replica (last error: %v)", lastErr)
}

// openEC streams an EC object through a pipe: data shards are joined, missing
// ones reconstructed from parity into in-memory staging. Each shard lookup
// falls back across epochs. Decode errors (including ErrUnrecoverable)
// surface through the returned reader.
func (c *Coordinator) openEC(ctx context.Context, plans []epochPlan, sc *Sidecar, s scheme.Scheme) io.ReadCloser {
	open := func(index int) (io.ReadCloser, error) { //nolint:unparam // OpenFunc contract: (nil, nil) marks a lost shard.
		rc, err := c.openFragment(ctx, plans, sc, index)
		if err != nil {
			// Lost shard: reconstruction decides whether enough survive.
			return nil, nil //nolint:nilnil // (nil, nil) is the OpenFunc "lost" contract.
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
// placement target, with candidates spanning every remembered topology epoch
// (so a topology change never makes a committed object unreachable) plus the
// bucket's configured scheme and the default layouts (for objects written
// under an earlier bucket scheme). All-absent means ErrNotFound; when every
// candidate errored otherwise, that error surfaces (an unreachable cluster
// must not read as "object gone").
func (c *Coordinator) fetchSidecar(ctx context.Context, bucket, key string) (*Sidecar, error) {
	name := sidecarName(bucket, key)

	var lastErr error

	for _, ct := range c.allSidecarCandidates(bucket, key) {
		p, err := c.dial(ct.topo, ct.target.Node)
		if err != nil {
			lastErr = err
			continue
		}

		rc, _, err := p.Get(ctx, ct.target.Disk, name)
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

// sidecarCandidates lists the targets that may hold the object's sidecar
// under one topology, in placement order: the bucket's configured scheme
// first, then the 3-target replica layout and the default EC layout for
// objects written under an earlier bucket scheme.
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
