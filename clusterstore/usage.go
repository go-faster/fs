package clusterstore

import (
	"context"
	"strings"
)

// UsageObserver receives per-bucket object accounting: how many objects and
// bytes a completed operation added or removed.
//
// It is called after the operation has committed, from the request goroutine,
// and must not block on anything slow — accounting is bookkeeping about a write
// that already succeeded, so it can never be allowed to fail one. The etcd
// implementation batches deltas and flushes them on its own schedule.
//
// Deltas are best-effort by construction. A node that dies between committing a
// write and reporting it leaves the counters short, and nothing here pretends
// otherwise: CountObjects is the authority, and a periodic recount is what makes
// the record true again.
type UsageObserver interface {
	Observe(bucket string, objects, bytes int64)
}

// BucketTotals is what one bucket holds.
type BucketTotals struct {
	Objects int64
	Bytes   int64
}

// internalBucket reports whether a namespace is coordinator bookkeeping rather
// than a user bucket — the multipart upload and part namespaces (see
// uploadsBucket and partsBucket), which are prefixed with a NUL that no S3
// bucket name can contain.
//
// They must not be accounted for. Parts of an upload in flight are not objects:
// S3 does not list them, does not charge a bucket for them until the upload
// completes, and completion writes the assembled object through the normal path
// — which is counted. Counting parts too would double every multipart write and
// leave an abandoned upload inflating a bucket forever.
func internalBucket(bucket string) bool { return strings.HasPrefix(bucket, "\x00") }

// observeUsage reports an accounting delta, if anything is listening.
func (c *Coordinator) observeUsage(bucket string, objects, bytes int64) {
	if c.usage == nil || internalBucket(bucket) {
		return
	}

	c.usage.Observe(bucket, objects, bytes)
}

// CountObjects totals every committed object in the cluster, per bucket.
//
// This is the authoritative count and the expensive one: it is a scatter-gather
// over every disk that reads every sidecar, the same walk a listing does, for
// every bucket at once. One pass answers the whole cluster, which is why the
// recount is scheduled cluster-wide rather than per bucket.
//
// Objects reachable only through an unreachable node are missed, so a recount
// taken during an outage undercounts. It is still an improvement on drifted
// counters, and the next recount corrects it.
func (c *Coordinator) CountObjects(ctx context.Context) (map[string]BucketTotals, error) {
	// Dedup by bucket and key: an object's sidecar is replicated across its
	// placement targets, so the same object is found once per target, and the
	// newest write wins exactly as it does in a listing.
	recs, err := gatherRecords(ctx, c, "obj/",
		func(data []byte) (string, *Sidecar, error) {
			sc, err := decodeSidecar(data)
			if err != nil {
				return "", nil, err
			}

			return objectRef(sc.Bucket, sc.Key), sc, nil
		},
		func(existing, candidate *Sidecar) bool {
			return candidate.Supersedes(existing)
		},
	)
	if err != nil {
		return nil, err
	}

	totals := make(map[string]BucketTotals)

	for _, sc := range recs {
		if internalBucket(sc.Bucket) {
			continue
		}

		t := totals[sc.Bucket]
		t.Objects++
		t.Bytes += sc.Size
		totals[sc.Bucket] = t
	}

	return totals, nil
}

// CountObjectsIndexed totals every committed object per bucket, from the nodes'
// object indexes rather than from their sidecars.
//
// This is the same count CountObjects produces and the same merge a listing
// uses — replicas of an object collapse to one entry, newest wins — but it
// reads index pages instead of scanning every disk and opening every sidecar.
// That is what makes re-deriving a bucket's totals cheap enough to do often,
// which matters more than the constant: counters corrected every hour drift
// less than counters corrected twice a day.
//
// It returns ErrIndexUnavailable when the indexes cannot answer — a node still
// building one, or running a binary without one — and the caller falls back to
// the walk.
func (c *Coordinator) CountObjectsIndexed(ctx context.Context) (map[string]BucketTotals, error) {
	buckets, err := c.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}

	totals := make(map[string]BucketTotals, len(buckets))

	for _, b := range buckets {
		if internalBucket(b.Name) {
			continue
		}

		total, err := c.countBucketIndexed(ctx, b.Name)
		if err != nil {
			return nil, err
		}

		totals[b.Name] = total
	}

	return totals, nil
}

// countBucketIndexed pages one bucket through the merged indexes.
func (c *Coordinator) countBucketIndexed(ctx context.Context, bucket string) (BucketTotals, error) {
	var (
		total BucketTotals
		after string
	)

	for {
		objects, _, more, err := c.ListPage(ctx, bucket, "", "", after, countPage)
		if err != nil {
			return BucketTotals{}, err
		}

		for _, sc := range objects {
			total.Objects++
			total.Bytes += sc.Size
		}

		if !more {
			return total, nil
		}

		if len(objects) == 0 {
			// Truncated with nothing to resume from would loop forever; treat
			// it as the end rather than spin.
			return total, nil
		}

		after = objects[len(objects)-1].Key
	}
}

// countPage is how many objects one page of a count carries. Larger than a
// listing page because nothing renders it: the only cost of a big page is the
// memory it occupies while being summed.
const countPage = 1000
