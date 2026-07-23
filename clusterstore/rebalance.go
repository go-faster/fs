package clusterstore

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/go-faster/errors"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs/internal/cluster"
)

// RebalanceCursor marks progress through the rebalance walk: every object up
// to and including (Bucket, Key) — in bucket order, key order within a bucket
// — has been processed. The zero value means "start from the beginning".
type RebalanceCursor struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// Encode serializes the cursor for persistence (etcd).
func (c RebalanceCursor) Encode() (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", errors.Wrap(err, "encode rebalance cursor")
	}

	return string(data), nil
}

// DecodeRebalanceCursor parses a persisted cursor.
func DecodeRebalanceCursor(s string) (RebalanceCursor, error) {
	var c RebalanceCursor
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return RebalanceCursor{}, errors.Wrap(err, "decode rebalance cursor")
	}

	return c, nil
}

// defaultRebalanceConcurrency is how many objects repair in parallel when
// RebalanceOptions.Concurrency is unset.
const defaultRebalanceConcurrency = 4

// RebalanceOptions configures one Rebalance pass.
type RebalanceOptions struct {
	// Resume skips every object at or before this cursor. Zero value walks
	// everything.
	Resume RebalanceCursor
	// Concurrency is how many objects repair in parallel (default 4). Peer
	// bandwidth is throttled separately; see ThrottledPeers.
	Concurrency int
	// Checkpoint, if set, persists the cursor after each completed batch: every
	// object at or before it is done, so a successor resumes there. A
	// checkpoint error aborts the pass (the runner has likely lost its
	// single-runner slot).
	Checkpoint func(ctx context.Context, cur RebalanceCursor) error
	// OnObject observes each processed object; rep is nil when err is set.
	// May be nil.
	OnObject func(bucket, key string, rep *RepairReport, err error)
}

// RebalanceReport summarizes one Rebalance pass.
type RebalanceReport struct {
	// Buckets is how many buckets were walked.
	Buckets int
	// Objects is how many objects were fed through repair.
	Objects int
	// Relocated counts objects where repair changed anything (fragments moved,
	// sidecars rewritten, old copies retired).
	Relocated int
	// Failed counts objects whose repair errored (also reported to OnError and
	// OnObject); the pass continues past them.
	Failed int
	// Totals aggregates the per-object repair actions.
	Totals RepairReport
}

// Rebalance walks every bucket's objects in key order and repairs each at the
// current epoch's placement — the manual rebalance pass (ROADMAP Phase 8).
// Relocation is the repair engine's copy → verify → delete: an object is never
// below its protection level mid-move. Objects are processed in batches of
// Concurrency with the cursor checkpointed between batches, so a killed runner
// is resumed by a successor without re-walking finished work (re-repairing an
// already-healthy object is a no-op).
func (r *Repairer) Rebalance(ctx context.Context, opts RebalanceOptions) (*RebalanceReport, error) {
	workers := opts.Concurrency
	if workers <= 0 {
		workers = defaultRebalanceConcurrency
	}

	buckets, err := r.coord.ListBuckets(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list buckets")
	}

	report := &RebalanceReport{}

	for _, b := range buckets {
		if b.Name < opts.Resume.Bucket {
			continue
		}

		scs, err := r.coord.ListObjects(ctx, b.Name, "")
		if err != nil {
			return report, errors.Wrapf(err, "list bucket %q", b.Name)
		}

		// Resume inside the cursor's bucket: everything at or before the key is
		// done. ListObjects returns keys sorted.
		if b.Name == opts.Resume.Bucket {
			for len(scs) > 0 && scs[0].Key <= opts.Resume.Key {
				scs = scs[1:]
			}
		}

		report.Buckets++

		for start := 0; start < len(scs); start += workers {
			if err := ctx.Err(); err != nil {
				return report, err
			}

			batch := scs[start:min(start+workers, len(scs))]

			var mu sync.Mutex

			var g errgroup.Group

			for _, sc := range batch {
				g.Go(func() error {
					rep, err := r.RepairObject(ctx, b.Name, sc.Key)

					mu.Lock()
					defer mu.Unlock()

					report.Objects++

					if err != nil {
						// Per-object failures don't stop the walk; the object is
						// reported and the next pass retries it.
						report.Failed++

						r.onErr(b.Name, sc.Key, err)
					} else {
						if rep.Changed() {
							report.Relocated++
						}

						report.Totals.add(rep)
					}

					if opts.OnObject != nil {
						opts.OnObject(b.Name, sc.Key, rep, err)
					}

					return nil
				})
			}

			_ = g.Wait() // Workers never fail the group; ctx is checked per batch.

			if opts.Checkpoint != nil {
				cur := RebalanceCursor{Bucket: b.Name, Key: batch[len(batch)-1].Key}
				if err := opts.Checkpoint(ctx, cur); err != nil {
					return report, errors.Wrap(err, "checkpoint")
				}
			}
		}
	}

	return report, nil
}

// NodePlan is one destination node's share of a rebalance plan.
type NodePlan struct {
	// Objects is how many objects have at least one fragment to place on this
	// node.
	Objects int
	// Bytes is the fragment payload volume to place on this node.
	Bytes int64
}

// RebalancePlan is the dry-run summary of a rebalance: what data is not yet at
// the current epoch's placement and where it has to go.
type RebalancePlan struct {
	// Objects is how many objects were examined.
	Objects int
	// MisplacedObjects counts objects with at least one fragment absent from
	// its current-placement target.
	MisplacedObjects int
	// MisplacedBytes is the total fragment payload volume to move.
	MisplacedBytes int64
	// Unplannable counts objects whose current placement cannot be computed
	// (e.g. the topology cannot host the scheme); rebalance would skip them
	// too.
	Unplannable int
	// Nodes breaks the move volume down by destination node.
	Nodes map[cluster.NodeID]*NodePlan
}

// PlanRebalance computes the dry-run plan: it walks every object like
// Rebalance would, compares the current epoch's placement against what each
// target actually holds (a stat per fragment — no payload is read) and totals
// the objects and bytes each node would receive. Nothing is modified.
func (r *Repairer) PlanRebalance(ctx context.Context) (*RebalancePlan, error) {
	buckets, err := r.coord.ListBuckets(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list buckets")
	}

	plan := &RebalancePlan{Nodes: make(map[cluster.NodeID]*NodePlan)}

	for _, b := range buckets {
		scs, err := r.coord.ListObjects(ctx, b.Name, "")
		if err != nil {
			return plan, errors.Wrapf(err, "list bucket %q", b.Name)
		}

		for _, sc := range scs {
			if err := ctx.Err(); err != nil {
				return plan, err
			}

			plan.Objects++

			topo := r.coord.topo.Topology()

			_, items, peers, err := r.coord.planFor(topo, sc)
			if err != nil {
				plan.Unplannable++
				continue
			}

			var nodesHit []cluster.NodeID

			for i := range items {
				name := fragmentName(sc.Bucket, sc.Key, sc.Generation, items[i].Index)
				if size, err := peers[i].Stat(ctx, items[i].Target.Disk, name); err == nil && size == items[i].Size {
					continue // Already in place.
				}

				node := items[i].Target.Node

				np := plan.Nodes[node]
				if np == nil {
					np = &NodePlan{}
					plan.Nodes[node] = np
				}

				np.Bytes += items[i].Size
				plan.MisplacedBytes += items[i].Size

				if !slices.Contains(nodesHit, node) {
					nodesHit = append(nodesHit, node)
					np.Objects++
				}
			}

			if len(nodesHit) > 0 {
				plan.MisplacedObjects++
			}
		}
	}

	return plan, nil
}
