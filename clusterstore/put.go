package clusterstore

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags/checksums are MD5 by protocol.
	"encoding/hex"
	"io"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// asyncTimeout bounds one async remainder task (parity/trailing-replica
// production plus stale-generation cleanup).
const asyncTimeout = 5 * time.Minute

// PutRequest describes one object write.
type PutRequest struct {
	Bucket string
	Key    string
	// Size is the exact body length; the coordinator streams exactly this
	// many bytes.
	Size int64
	Body io.Reader

	Metadata fs.ObjectMetadata
	Tags     []fs.Tag
	ACL      fs.ACL
	// ETag overrides the stored ETag (multipart composite ETags); empty means
	// the content MD5.
	ETag string
}

// Put writes an object at its bucket's scheme, acknowledging only once the
// synchronous write quorum is durable on distinct failure domains: the first
// two full replicas for RF=2.5/RF=3, or all k+m shards for EC. The remainder
// (RF=2.5 parity, the RF=3 third replica) and stale-generation cleanup happen
// behind the async queue. A write that cannot reach quorum is refused and
// leaves no new committed state.
func (c *Coordinator) Put(ctx context.Context, req *PutRequest) (*Sidecar, error) {
	c.waitKey(req.Bucket, req.Key)

	topo := c.topo.Topology()
	s := c.schemeFor(req.Bucket)
	pkey := placement.ObjectKey(req.Bucket, req.Key)

	plan, err := fragment.Plan(topo, s, pkey, req.Size)
	if err != nil {
		return nil, err
	}

	peers, err := c.dialPlan(topo, plan)
	if err != nil {
		return nil, err
	}

	gen, err := newGeneration()
	if err != nil {
		return nil, err
	}

	// The previous committed state, for stale-generation cleanup and sidecar
	// rollback. Best-effort: an unreachable sidecar must not block the write.
	oldSC, _ := c.fetchSidecar(ctx, req.Bucket, req.Key)

	sink := func(item fragment.Item) (io.WriteCloser, error) {
		name := fragmentName(req.Bucket, req.Key, gen, item.Index)

		return newPutSink(ctx, peers[item.Index], item.Target.Disk, name, item.Size), nil
	}
	reopen := func(item fragment.Item) (io.ReadCloser, error) {
		rc, _, err := peers[item.Index].Get(ctx, item.Target.Disk, fragmentName(req.Bucket, req.Key, gen, item.Index))

		return rc, err
	}

	hasher := md5.New() //nolint:gosec // Content checksum, not a security primitive.
	body := io.TeeReader(req.Body, hasher)

	// Synchronous quorum phase. For EC every shard (data and parity) must land
	// before the ack — there is no safe async path to complete a shard set.
	if err := fragment.EncodeDataStream(plan, s, req.Size, body, sink); err != nil {
		c.discardGeneration(ctx, plan, peers, req.Bucket, req.Key, gen)
		return nil, errors.Wrap(err, "write quorum fragments")
	}

	if s.Kind == scheme.EC {
		if err := fragment.EncodeParityStream(plan, s, req.Size, sink, reopen); err != nil {
			c.discardGeneration(ctx, plan, peers, req.Bucket, req.Key, gen)
			return nil, errors.Wrap(err, "write EC parity shards")
		}
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	etag := req.ETag

	if etag == "" {
		etag = checksum
	}

	var seq int64 = 1
	if oldSC != nil {
		seq = oldSC.Seq + 1
	}

	sc := &Sidecar{
		Version:            sidecarVersion,
		Bucket:             req.Bucket,
		Key:                req.Key,
		Scheme:             s.String(),
		Size:               req.Size,
		Generation:         gen,
		Seq:                seq,
		Modified:           time.Now().UTC(),
		ETag:               etag,
		Checksum:           checksum,
		ContentType:        req.Metadata.ContentType,
		CacheControl:       req.Metadata.CacheControl,
		ContentDisposition: req.Metadata.ContentDisposition,
		ContentEncoding:    req.Metadata.ContentEncoding,
		UserMetadata:       req.Metadata.UserMetadata,
		Tags:               req.Tags,
		ACL:                req.ACL,
	}

	// Commit: replace the sidecar on every quorum target. This is what makes
	// the generation visible; a failure rolls the touched targets back to the
	// previous sidecar (or none) and refuses the write.
	if err := c.commitSidecar(ctx, plan, peers, sc, oldSC, s.WriteQuorum()); err != nil {
		c.discardGeneration(ctx, plan, peers, req.Bucket, req.Key, gen)
		return nil, err
	}

	c.enqueue(req.Bucket, req.Key, func() {
		bg, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()

		c.completeWrite(bg, topo, plan, peers, s, sc, oldSC)
	})

	return sc, nil
}

// UpdateSidecar rewrites an object's committed metadata in place (tags, ACL —
// anything that does not touch payload fragments): the sidecar is fetched,
// mutated, and re-replicated to the object's targets, quorum synchronously
// and the remainder best-effort. Concurrent Put/Update races on the same key
// are the caller's to serialize (the fs.Storage layer holds a per-key lock);
// cross-node races follow last-write-wins per target like every sidecar
// write.
func (c *Coordinator) UpdateSidecar(ctx context.Context, bucket, key string, mutate func(*Sidecar)) error {
	c.waitKey(bucket, key)

	topo := c.topo.Topology()

	sc, err := c.fetchSidecar(ctx, bucket, key)
	if err != nil {
		return err
	}

	s, plan, peers, err := c.planFor(topo, sc)
	if err != nil {
		return err
	}

	mutate(sc)

	data, err := sc.encode()
	if err != nil {
		return err
	}

	name := sidecarName(bucket, key)

	for i := range plan {
		err := putBytes(ctx, peers[i], plan[i].Target.Disk, name, data)
		if err == nil {
			continue
		}

		if i < s.WriteQuorum() {
			// Sub-quorum: already-updated targets diverge until repair, but
			// the caller learns the update did not durably land.
			return errors.Wrapf(err, "update sidecar on %s/%s", plan[i].Target.Node, plan[i].Target.Disk)
		}

		c.onErr(bucket, key, errors.Wrapf(err, "extend sidecar update to %s/%s", plan[i].Target.Node, plan[i].Target.Disk))
	}

	return nil
}

// dialPlan resolves the peer for every planned fragment, indexed by
// fragment index.
func (c *Coordinator) dialPlan(topo *cluster.Topology, plan []fragment.Item) ([]Peer, error) {
	peers := make([]Peer, len(plan))

	for i, item := range plan {
		p, err := c.dial(topo, item.Target.Node)
		if err != nil {
			return nil, err
		}

		peers[i] = p
	}

	return peers, nil
}

// commitSidecar writes the sidecar to the first quorum targets sequentially,
// rolling touched targets back to prev (or deleting) on failure.
func (c *Coordinator) commitSidecar(ctx context.Context, plan []fragment.Item, peers []Peer, sc, prev *Sidecar, quorum int) error {
	data, err := sc.encode()
	if err != nil {
		return err
	}

	name := sidecarName(sc.Bucket, sc.Key)

	for i := range quorum {
		if err := putBytes(ctx, peers[i], plan[i].Target.Disk, name, data); err != nil {
			c.rollbackSidecar(ctx, plan[:i], peers[:i], name, prev)
			return errors.Wrapf(err, "commit sidecar to %s/%s", plan[i].Target.Node, plan[i].Target.Disk)
		}
	}

	return nil
}

// rollbackSidecar restores the previous sidecar (or removes the new one) on
// targets already committed before a mid-quorum failure. Best-effort: targets
// it cannot reach diverge until repair.
func (c *Coordinator) rollbackSidecar(ctx context.Context, plan []fragment.Item, peers []Peer, name string, prev *Sidecar) {
	var data []byte

	if prev != nil {
		var err error
		if data, err = prev.encode(); err != nil {
			prev = nil
		}
	}

	for i := range plan {
		if prev != nil {
			_ = putBytes(ctx, peers[i], plan[i].Target.Disk, name, data)
			continue
		}

		_ = peers[i].Delete(ctx, plan[i].Target.Disk, name)
	}
}

// discardGeneration best-effort deletes a generation's fragments after a
// refused write; anything unreachable becomes scrubber garbage, never a
// visible object (the sidecar was not committed).
func (c *Coordinator) discardGeneration(ctx context.Context, plan []fragment.Item, peers []Peer, bucket, key, gen string) {
	for i := range plan {
		_ = peers[i].Delete(ctx, plan[i].Target.Disk, fragmentName(bucket, key, gen, plan[i].Index))
	}
}

// completeWrite is the async remainder of an acknowledged write: produce the
// non-quorum fragments (RF=2.5 parity or the RF=3 trailing replica), extend
// the sidecar to the remaining targets, then clean up the previous
// generation. Failures are reported to the OnAsyncError hook — the object is
// already durable at quorum and the repair worker finishes the job.
func (c *Coordinator) completeWrite(ctx context.Context, topo *cluster.Topology, plan []fragment.Item, peers []Peer, s scheme.Scheme, sc, oldSC *Sidecar) {
	sink := func(item fragment.Item) (io.WriteCloser, error) {
		name := fragmentName(sc.Bucket, sc.Key, sc.Generation, item.Index)

		return newPutSink(ctx, peers[item.Index], item.Target.Disk, name, item.Size), nil
	}
	reopen := func(item fragment.Item) (io.ReadCloser, error) {
		rc, _, err := peers[item.Index].Get(ctx, item.Target.Disk, fragmentName(sc.Bucket, sc.Key, sc.Generation, item.Index))

		return rc, err
	}

	if s.Kind != scheme.EC {
		if err := fragment.EncodeParityStream(plan, s, sc.Size, sink, reopen); err != nil {
			c.onErr(sc.Bucket, sc.Key, errors.Wrap(err, "async remainder fragment"))
			return
		}

		data, err := sc.encode()
		if err != nil {
			c.onErr(sc.Bucket, sc.Key, err)
			return
		}

		name := sidecarName(sc.Bucket, sc.Key)

		for i := s.WriteQuorum(); i < len(plan); i++ {
			if err := putBytes(ctx, peers[i], plan[i].Target.Disk, name, data); err != nil {
				c.onErr(sc.Bucket, sc.Key, errors.Wrapf(err, "extend sidecar to %s/%s", plan[i].Target.Node, plan[i].Target.Disk))
				return
			}
		}
	}

	if oldSC != nil {
		if err := c.cleanupGeneration(ctx, topo, plan, oldSC); err != nil {
			c.onErr(sc.Bucket, sc.Key, errors.Wrap(err, "clean up previous generation"))
		}
	}
}

// cleanupGeneration removes a superseded generation's fragments across every
// remembered epoch's placement, and — on old targets no longer covered by the
// new placement (a scheme or topology change moved the target set) — its
// sidecar, so no phantom copy survives.
func (c *Coordinator) cleanupGeneration(ctx context.Context, _ *cluster.Topology, newPlan []fragment.Item, old *Sidecar) error {
	oldScheme, err := old.ParseScheme()
	if err != nil {
		return err
	}

	covered := make(map[diskRef]struct{}, len(newPlan))
	for _, item := range newPlan {
		covered[targetRef(item.Target)] = struct{}{}
	}

	var firstErr error

	// NB: dedup fragment deletes by (disk, index) — the same disk can hold a
	// DIFFERENT index of the object under another epoch's placement, and a
	// disk-only dedup would leave that fragment behind.
	type fragRef struct {
		ref diskRef
		idx int
	}

	fragSeen := make(map[fragRef]struct{})
	metaSeen := make(map[diskRef]struct{})

	for _, ep := range c.epochPlans(oldScheme, old.Bucket, old.Key, old.Size) {
		for _, item := range ep.plan {
			p, err := c.dial(ep.topo, item.Target.Node)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}

				continue
			}

			fr := fragRef{ref: targetRef(item.Target), idx: item.Index}
			if _, done := fragSeen[fr]; !done {
				fragSeen[fr] = struct{}{}

				name := fragmentName(old.Bucket, old.Key, old.Generation, item.Index)
				if err := p.Delete(ctx, item.Target.Disk, name); err != nil && !errors.Is(err, transport.ErrNotFound) {
					if firstErr == nil {
						firstErr = err
					}
				}
			}

			if _, ok := covered[targetRef(item.Target)]; ok {
				continue
			}

			if _, done := metaSeen[targetRef(item.Target)]; done {
				continue
			}

			metaSeen[targetRef(item.Target)] = struct{}{}

			// Target dropped out of the new placement: its old sidecar would
			// keep serving the stale generation.
			if err := p.Delete(ctx, item.Target.Disk, sidecarName(old.Bucket, old.Key)); err != nil && !errors.Is(err, transport.ErrNotFound) {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}

	return firstErr
}

// putBytes stores a small in-memory payload (sidecars) on a peer.
func putBytes(ctx context.Context, p Peer, disk cluster.DiskID, name string, data []byte) error {
	return p.Put(ctx, disk, name, int64(len(data)), bytes.NewReader(data))
}

// pipeSink adapts Peer.Put (a reader-consuming call) to the io.WriteCloser
// the fragment encoder feeds: bytes written stream into the peer request, and
// Close reports the peer's verified ack.
type pipeSink struct {
	pw   *io.PipeWriter
	done chan error
}

// newPutSink starts the peer upload and returns the writer feeding it.
func newPutSink(ctx context.Context, p Peer, disk cluster.DiskID, name string, size int64) io.WriteCloser {
	pr, pw := io.Pipe()
	done := make(chan error, 1)

	go func() {
		err := p.Put(ctx, disk, name, size, pr)
		// Unblock the producer on failure; a clean Close otherwise.
		if err != nil {
			_ = pr.CloseWithError(err)
		} else {
			_ = pr.Close()
		}

		done <- err
	}()

	return &pipeSink{pw: pw, done: done}
}

func (s *pipeSink) Write(p []byte) (int, error) { return s.pw.Write(p) }

// Close finishes the stream and waits for the peer's ack.
func (s *pipeSink) Close() error {
	_ = s.pw.Close()

	return <-s.done
}
