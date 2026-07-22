package clusterstore

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// ListObjects gathers the objects of a bucket with the given key prefix:
// every disk in the cluster is scanned for committed sidecars under the
// bucket's namespace and the results are merged by key, newest write wins
// (Modified, then generation as the tie-break for equal stamps). The listing
// is sorted by key.
//
// Per-target failures are tolerated: an object's sidecar is replicated across
// its placement targets, so a listing stays complete while every object keeps
// at least one reachable sidecar (the same availability bound as reads). Only
// when every scan fails does the error surface. Listing reads each sidecar
// individually; a per-node listing index is a later optimization.
func (c *Coordinator) ListObjects(ctx context.Context, bucket, prefix string) ([]*Sidecar, error) {
	recs, err := gatherRecords(ctx, c, bucketObjectsPrefix(bucket),
		func(data []byte) (string, *Sidecar, error) {
			sc, err := decodeSidecar(data)
			if err != nil {
				return "", nil, err
			}

			return sc.Key, sc, nil
		},
		func(existing, candidate *Sidecar) bool {
			if !candidate.Modified.Equal(existing.Modified) {
				return candidate.Modified.After(existing.Modified)
			}

			return candidate.Generation > existing.Generation
		},
	)
	if err != nil {
		return nil, err
	}

	out := make([]*Sidecar, 0, len(recs))

	for _, sc := range recs {
		if strings.HasPrefix(sc.Key, prefix) {
			out = append(out, sc)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })

	return out, nil
}

// gatherRecords is the scatter-gather engine behind listings: it fans out
// over every (node, disk), lists the "/meta" records under prefix, fetches
// and decodes each, and merges them by the decoder's dedup key. better
// decides whether a candidate replaces the record already gathered under the
// same key (nil keeps the first). Individual scan failures are tolerated;
// when every scan fails, the first error surfaces.
func gatherRecords[T any](
	ctx context.Context,
	c *Coordinator,
	prefix string,
	decode func(data []byte) (string, T, error),
	better func(existing, candidate T) bool,
) (map[string]T, error) {
	topo := c.topo.Topology()

	var (
		mu       sync.Mutex
		recs     = make(map[string]T)
		scans    int
		failures int
		firstErr error
	)

	fail := func(err error) {
		failures++

		if firstErr == nil {
			firstErr = err
		}
	}

	var wg sync.WaitGroup

	for i := range topo.Nodes {
		node := topo.Nodes[i]
		scans += len(node.Disks)

		wg.Go(func() {
			p, err := c.peers.Peer(node)
			if err != nil {
				mu.Lock()
				for range node.Disks {
					fail(err)
				}
				mu.Unlock()

				return
			}

			for _, disk := range node.Disks {
				gatherDisk(ctx, p, disk.ID, prefix, func(data []byte) {
					key, rec, err := decode(data)
					if err != nil {
						return // A corrupt record replica; another copy serves.
					}

					mu.Lock()
					if existing, ok := recs[key]; !ok || (better != nil && better(existing, rec)) {
						recs[key] = rec
					}
					mu.Unlock()
				}, func(err error) {
					mu.Lock()
					fail(err)
					mu.Unlock()
				})
			}
		})
	}

	wg.Wait()

	if scans > 0 && failures == scans {
		return nil, errors.Wrap(firstErr, "every listing scan failed")
	}

	return recs, nil
}

// gatherDisk lists one disk's records under prefix and feeds each fetched
// record to sink. A listing failure aborts the disk via fail; individual
// record fetch failures are skipped (racing deletes, replicas elsewhere).
func gatherDisk(ctx context.Context, p Peer, disk cluster.DiskID, prefix string, sink func(data []byte), fail func(error)) {
	names, err := p.List(ctx, disk, prefix)
	if err != nil {
		fail(err)
		return
	}

	for _, name := range names {
		if !strings.HasSuffix(name, "/meta") {
			continue
		}

		rc, _, err := p.Get(ctx, disk, name)
		if err != nil {
			continue
		}

		data, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			continue
		}

		sink(data)
	}
}
