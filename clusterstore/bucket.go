package clusterstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// ErrSchemeRejected matches (via errors.Is) any error SetBucketScheme returns
// because the requested scheme does not parse or the current topology cannot
// host it. It is a client error: the request named a scheme this cluster
// cannot serve, not a control-plane failure.
var ErrSchemeRejected = errors.New("scheme rejected")

// schemeRejected tags an error as a scheme rejection while keeping the
// underlying reason (an unparseable scheme, or a placement error such as
// ErrInsufficientTargets) in the chain for errors.Is.
type schemeRejected struct{ err error }

func (e schemeRejected) Error() string        { return e.err.Error() }
func (e schemeRejected) Unwrap() error        { return e.err }
func (e schemeRejected) Is(target error) bool { return target == ErrSchemeRejected }

// bucketCopies is how many targets hold a bucket record. Bucket records are
// tiny and rarely written, so they always use 3-way replication regardless of
// the bucket's object scheme.
const bucketCopies = 3

// bucketQuorum is the number of record replicas that must be durable before a
// bucket mutation is acknowledged; the third is best-effort (repair completes
// it).
const bucketQuorum = 2

// BucketInfo is the replicated bucket record: the cluster's source of truth
// that a bucket exists, plus its bucket-level S3 state. True cluster-wide
// create/delete linearizability arrives with the etcd control plane; until
// then concurrent conflicting mutations from different nodes follow
// last-write-wins per target, like object sidecars.
type BucketInfo struct {
	Version int       `json:"version"`
	Name    string    `json:"name"`
	ACL     fs.ACL    `json:"acl,omitempty"`
	Created time.Time `json:"created"`
	// Scheme overrides the cluster default replication scheme for this
	// bucket's objects ("rf2.5", "rf3", "ec:k,m"); empty applies the default.
	// Changing it affects new writes immediately; existing objects follow
	// through scheme conversion in repair/rebalance (ROADMAP Phase 8).
	Scheme string `json:"scheme,omitempty"`
}

// bucketRecordName is the store name of a bucket's record; like objects, the
// human-readable name lives inside the record and the path is a hash.
func bucketRecordName(bucket string) string {
	sum := sha256.Sum256([]byte(bucket))

	return "bkt/" + hex.EncodeToString(sum[:]) + "/meta"
}

// bucketPlacementKey isolates bucket records from the object key space.
func bucketPlacementKey(bucket string) string {
	return "\x00bkt\x00" + bucket
}

// bucketTargets resolves the placement of a bucket's record replicas.
func (c *Coordinator) bucketTargets(topo *cluster.Topology, bucket string) ([]placement.Target, []Peer, error) {
	targets := placement.Place(topo, bucketPlacementKey(bucket), bucketCopies)
	if len(targets) < bucketQuorum {
		return nil, nil, errors.Wrapf(ErrInsufficientTargets, "bucket record needs %d, got %d", bucketQuorum, len(targets))
	}

	peers := make([]Peer, len(targets))

	for i, t := range targets {
		p, err := c.dial(topo, t.Node)
		if err != nil {
			return nil, nil, err
		}

		peers[i] = p
	}

	return targets, peers, nil
}

// CreateBucket records a new bucket, refusing when it already exists
// (fs.ErrBucketAlreadyExists). The existence check and the write are not
// atomic across nodes until the etcd control plane lands; a racing duplicate
// create converges to a single record.
func (c *Coordinator) CreateBucket(ctx context.Context, bucket string, acl fs.ACL) error {
	topo := c.topo.Topology()

	switch _, err := c.fetchBucket(ctx, topo, bucket); {
	case err == nil:
		return errors.Wrap(fs.ErrBucketAlreadyExists, bucket)
	case !errors.Is(err, fs.ErrBucketNotFound):
		return err
	}

	return c.writeBucket(ctx, topo, &BucketInfo{
		Version: sidecarVersion,
		Name:    bucket,
		ACL:     acl,
		Created: time.Now().UTC(),
	})
}

// writeBucket replicates a bucket record: quorum targets synchronously, the
// remainder best-effort.
func (c *Coordinator) writeBucket(ctx context.Context, topo *cluster.Topology, info *BucketInfo) error {
	targets, peers, err := c.bucketTargets(topo, info.Name)
	if err != nil {
		return err
	}

	data, err := json.Marshal(info)
	if err != nil {
		return errors.Wrap(err, "marshal bucket record")
	}

	name := bucketRecordName(info.Name)

	for i := range targets {
		err := putBytes(ctx, peers[i], targets[i].Disk, name, data)
		if err == nil {
			continue
		}

		if i < bucketQuorum {
			// Sub-quorum: roll back and refuse.
			for j := range i {
				_ = peers[j].Delete(ctx, targets[j].Disk, name)
			}

			return errors.Wrapf(err, "write bucket record to %s/%s", targets[i].Node, targets[i].Disk)
		}

		c.onErr(info.Name, "", errors.Wrapf(err, "extend bucket record to %s/%s", targets[i].Node, targets[i].Disk))
	}

	return nil
}

// DeleteBucket removes a bucket's record from every target. The caller
// (the fs.Storage layer) is responsible for the emptiness check.
func (c *Coordinator) DeleteBucket(ctx context.Context, bucket string) error {
	topo := c.topo.Topology()

	if _, err := c.fetchBucket(ctx, topo, bucket); err != nil {
		return err
	}

	targets, peers, err := c.bucketTargets(topo, bucket)
	if err != nil {
		return err
	}

	name := bucketRecordName(bucket)

	var firstErr error

	for i := range targets {
		if err := peers[i].Delete(ctx, targets[i].Disk, name); err != nil && !errors.Is(err, transport.ErrNotFound) {
			if firstErr == nil {
				firstErr = errors.Wrapf(err, "delete bucket record on %s/%s", targets[i].Node, targets[i].Disk)
			}
		}
	}

	return firstErr
}

// BucketExists reports whether the bucket record exists.
func (c *Coordinator) BucketExists(ctx context.Context, bucket string) (bool, error) {
	switch _, err := c.fetchBucket(ctx, c.topo.Topology(), bucket); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrBucketNotFound):
		return false, nil
	default:
		return false, err
	}
}

// Bucket returns the bucket record, or fs.ErrBucketNotFound.
func (c *Coordinator) Bucket(ctx context.Context, bucket string) (*BucketInfo, error) {
	return c.fetchBucket(ctx, c.topo.Topology(), bucket)
}

// SetBucketACL rewrites the bucket record with a new ACL.
func (c *Coordinator) SetBucketACL(ctx context.Context, bucket string, acl fs.ACL) error {
	topo := c.topo.Topology()

	info, err := c.fetchBucket(ctx, topo, bucket)
	if err != nil {
		return err
	}

	info.ACL = acl

	return c.writeBucket(ctx, topo, info)
}

// SetBucketScheme rewrites the bucket record with a new object scheme
// override; empty restores the cluster default. The scheme must parse and the
// current topology must be able to host it (a bucket must never be switched
// into a scheme its cluster cannot write). Existing objects converge to the
// new scheme through repair/rebalance conversion.
func (c *Coordinator) SetBucketScheme(ctx context.Context, bucket, schemeID string) error {
	topo := c.topo.Topology()

	if schemeID != "" {
		s, err := scheme.Parse(schemeID)
		if err != nil {
			return schemeRejected{errors.Wrap(err, "parse bucket scheme")}
		}

		schemeID = s.String() // Normalized form.

		if _, err := fragment.Plan(topo, s, placement.ObjectKey(bucket, "\x00scheme-probe"), 1); err != nil {
			return schemeRejected{errors.Wrapf(err, "topology cannot host scheme %s", schemeID)}
		}
	}

	info, err := c.fetchBucket(ctx, topo, bucket)
	if err != nil {
		return err
	}

	info.Scheme = schemeID

	if err := c.writeBucket(ctx, topo, info); err != nil {
		return err
	}

	// This node's writes and conversions see the change immediately; peers
	// converge within the scheme cache TTL.
	c.cacheBucketScheme(bucket, c.parseBucketScheme(info))

	return nil
}

// fetchBucket reads the bucket record from the first reachable target. Like
// object sidecars, all-absent is fs.ErrBucketNotFound while an unreachable
// cluster surfaces its error.
func (c *Coordinator) fetchBucket(ctx context.Context, topo *cluster.Topology, bucket string) (*BucketInfo, error) {
	targets, peers, err := c.bucketTargets(topo, bucket)
	if err != nil {
		return nil, err
	}

	name := bucketRecordName(bucket)

	var lastErr error

	for i := range targets {
		rc, _, err := peers[i].Get(ctx, targets[i].Disk, name)
		if err != nil {
			if !errors.Is(err, transport.ErrNotFound) {
				lastErr = err
			}

			continue
		}

		data, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			lastErr = errors.Wrap(err, "read bucket record")
			continue
		}

		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			lastErr = errors.Wrap(err, "unmarshal bucket record")
			continue
		}

		return &info, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, errors.Wrap(fs.ErrBucketNotFound, bucket)
}

// ListBuckets gathers the bucket records from every disk in the cluster,
// deduplicated by name and sorted. Per-target failures are tolerated as long
// as every record remains reachable on some replica; if every listing fails
// the error surfaces.
func (c *Coordinator) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	recs, err := gatherRecords(ctx, c, "bkt/", func(data []byte) (string, *BucketInfo, error) {
		var info BucketInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return "", nil, errors.Wrap(err, "unmarshal bucket record")
		}

		return info.Name, &info, nil
	}, nil)
	if err != nil {
		return nil, err
	}

	out := make([]BucketInfo, 0, len(recs))
	for _, info := range recs {
		out = append(out, *info)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}
