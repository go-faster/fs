package shardstore

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// Serve answers a peer's shard request from this node's shard.
//
// The whole of the server side. It is a translation and nothing else, which is
// the point: a remote caller gets the same shard, with the same ownership
// refusals, and no behavior lives out here where only one of the two paths
// would exercise it.
func Serve(shard *Shard) transport.ShardFunc {
	return func(ctx context.Context, req transport.ShardRequest) (transport.ShardResponse, error) {
		switch req.Op {
		case transport.ShardOpPut:
			if req.Entry == nil {
				return transport.ShardResponse{}, errors.New("put without an entry")
			}

			return notOwned(shard.Put(ctx, *req.Entry))

		case transport.ShardOpDelete:
			return notOwned(shard.Delete(ctx, req.Bucket, req.Key))

		case transport.ShardOpGet:
			entry, found, err := shard.Get(ctx, req.Bucket, req.Key)
			if resp, handled := ownership(err); handled {
				return resp, nil
			}

			if err != nil {
				return transport.ShardResponse{}, err
			}

			if !found {
				return transport.ShardResponse{}, nil
			}

			return transport.ShardResponse{Found: true, Entry: &entry}, nil

		case transport.ShardOpUsage:
			usage, err := shard.Usage(ctx, req.Bucket)
			if err != nil {
				return transport.ShardResponse{}, err
			}

			return transport.ShardResponse{Usage: &usage}, nil

		case transport.ShardOpBuckets:
			var names []string

			if err := shard.Buckets(ctx, func(bucket string) error {
				names = append(names, bucket)

				return nil
			}); err != nil {
				return transport.ShardResponse{}, err
			}

			return transport.ShardResponse{Buckets: names}, nil

		case transport.ShardOpScan:
			if req.Range == nil {
				return transport.ShardResponse{}, errors.New("scan without a range")
			}

			var entries []metastore.Entry

			err := shard.ScanRange(ctx, *req.Range, req.Bucket, req.Prefix, req.After, req.Limit,
				func(e metastore.Entry) error {
					entries = append(entries, e)

					return nil
				})
			if resp, handled := ownership(err); handled {
				return resp, nil
			}

			if err != nil {
				return transport.ShardResponse{}, err
			}

			return transport.ShardResponse{Entries: entries}, nil

		case transport.ShardOpVerified:
			return transport.ShardResponse{}, shard.SetVerified(ctx, req.Records)

		case transport.ShardOpCoverage:
			cov, err := shard.Coverage(ctx)
			if err != nil {
				return transport.ShardResponse{}, err
			}

			return transport.ShardResponse{Coverage: &cov}, nil

		case transport.ShardOpApply:
			if req.Range == nil {
				return transport.ShardResponse{}, errors.New("apply without a range")
			}

			err := shard.ApplyBatch(ctx, *req.Range, req.Batch)
			if errors.Is(err, ErrNotFollowed) {
				return transport.ShardResponse{NotFollowed: true}, nil
			}

			return transport.ShardResponse{}, err

		case transport.ShardOpMeasure:
			if req.Range == nil {
				return transport.ShardResponse{}, errors.New("measure without a range")
			}

			size, err := shard.RangeSize(*req.Range)
			if err != nil {
				return transport.ShardResponse{}, err
			}

			// The split point is asked for alongside the size rather than in a
			// second round trip. They are read from the same table metadata, and
			// a controller that had the size would ask for the point next
			// anyway — on a map that may have changed in between.
			at, _, err := shard.SplitPoint(*req.Range)
			if err != nil {
				return transport.ShardResponse{}, err
			}

			return transport.ShardResponse{Bytes: size.Bytes, SplitAt: at}, nil

		case transport.ShardOpBackfill:
			if req.Range == nil {
				return transport.ShardResponse{}, errors.New("backfill without a range")
			}

			step, err := shard.ReadBackfill(ctx, *req.Range, req.Cursor, req.Limit)
			if resp, handled := ownership(err); handled {
				return resp, nil
			}

			if err != nil {
				return transport.ShardResponse{}, err
			}

			return transport.ShardResponse{
				Entries: step.Entries,
				Cursor:  step.Cursor,
				Done:    step.Done,
			}, nil

		case transport.ShardOpReset:
			return transport.ShardResponse{}, shard.Reset(ctx)

		default:
			// Reported as unsupported rather than as a failure: a newer caller
			// asking an older peer for an operation it does not implement
			// should degrade the same way a peer without a shard at all does.
			return transport.ShardResponse{}, errors.Wrapf(
				transport.ErrUnknownShardOp, "shard operation %q", req.Op)
		}
	}
}

// ownership turns a shard's refusal into an answer rather than a failure.
func ownership(err error) (transport.ShardResponse, bool) {
	if errors.Is(err, ErrNotOwned) {
		return transport.ShardResponse{NotOwned: true}, true
	}

	return transport.ShardResponse{}, false
}

// notOwned is ownership for the operations that return only an error.
func notOwned(err error) (transport.ShardResponse, error) {
	if resp, handled := ownership(err); handled {
		return resp, nil
	}

	return transport.ShardResponse{}, err
}

// Peer is a shard on another node, reached over the peer transport.
//
// It satisfies Backend, so the Store's fan-out neither knows nor cares whether
// a shard is local. That is what let the composition be written and tested
// before any of this existed — and it is why nothing about ordering, summing or
// paging appears here.
type Peer struct {
	client *transport.Client
}

var _ Backend = (*Peer)(nil)

// NewPeer wraps a peer transport client as a shard backend.
func NewPeer(client *transport.Client) *Peer { return &Peer{client: client} }

// call runs one operation, restoring the shard's ownership sentinel on the way
// back so a remote refusal is indistinguishable from a local one.
func (p *Peer) call(ctx context.Context, req transport.ShardRequest) (transport.ShardResponse, error) {
	resp, err := p.client.Shard(ctx, req)
	if err != nil {
		return transport.ShardResponse{}, err
	}

	if resp.NotOwned {
		return transport.ShardResponse{}, ErrNotOwned
	}

	if resp.NotFollowed {
		return transport.ShardResponse{}, ErrNotFollowed
	}

	return resp, nil
}

// ApplyBatch ships a committed batch to this peer, which replays it as a
// follower of the range.
func (p *Peer) ApplyBatch(ctx context.Context, r rangemap.Range, repr []byte) error {
	_, err := p.call(ctx, transport.ShardRequest{
		Op:    transport.ShardOpApply,
		Range: &r,
		Batch: repr,
	})

	return err
}

// ReadBackfill reads one step of a range out of this peer, which owns it.
//
// The step arrives whole rather than streamed, for the reason the step exists:
// it is bounded so that a killed backfill repeats one step's work, and a bound
// small enough for that is small enough to carry in one response.
func (p *Peer) ReadBackfill(
	ctx context.Context,
	r rangemap.Range,
	cursor string,
	limit int,
) (BackfillStep, error) {
	resp, err := p.call(ctx, transport.ShardRequest{
		Op:     transport.ShardOpBackfill,
		Range:  &r,
		Cursor: cursor,
		Limit:  limit,
	})
	if err != nil {
		return BackfillStep{}, err
	}

	return BackfillStep{
		Entries: resp.Entries,
		Cursor:  resp.Cursor,
		Done:    resp.Done,
	}, nil
}

// Put implements Backend.
func (p *Peer) Put(ctx context.Context, e metastore.Entry) error {
	_, err := p.call(ctx, transport.ShardRequest{Op: transport.ShardOpPut, Entry: &e})

	return err
}

// Get implements Backend.
func (p *Peer) Get(ctx context.Context, bucket, key string) (metastore.Entry, bool, error) {
	resp, err := p.call(ctx, transport.ShardRequest{
		Op: transport.ShardOpGet, Bucket: bucket, Key: key,
	})
	if err != nil {
		return metastore.Entry{}, false, err
	}

	if !resp.Found || resp.Entry == nil {
		return metastore.Entry{}, false, nil
	}

	return *resp.Entry, true, nil
}

// Delete implements Backend.
func (p *Peer) Delete(ctx context.Context, bucket, key string) error {
	_, err := p.call(ctx, transport.ShardRequest{
		Op: transport.ShardOpDelete, Bucket: bucket, Key: key,
	})

	return err
}

// Usage implements Backend.
func (p *Peer) Usage(ctx context.Context, bucket string) (metastore.Usage, error) {
	resp, err := p.call(ctx, transport.ShardRequest{Op: transport.ShardOpUsage, Bucket: bucket})
	if err != nil {
		return metastore.Usage{}, err
	}

	if resp.Usage == nil {
		return metastore.Usage{}, nil
	}

	return *resp.Usage, nil
}

// Buckets implements Backend.
func (p *Peer) Buckets(ctx context.Context, fn func(bucket string) error) error {
	resp, err := p.call(ctx, transport.ShardRequest{Op: transport.ShardOpBuckets})
	if err != nil {
		return err
	}

	for _, bucket := range resp.Buckets {
		// Checked per item, not once per call. The page arrived whole, so
		// without this a caller that gives up mid-iteration is never told —
		// and a remote backend would honor cancellation less well than a
		// local one, which the conformance suite is entitled to reject.
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan buckets")
		}

		if err := fn(bucket); err != nil {
			return err
		}
	}

	return nil
}

// ScanRange implements Backend.
//
// The page arrives whole rather than streamed. It is bounded by the caller's
// limit and by the transport's own cap, which is what makes that safe — and a
// listing page is small by construction, because a page is what a client asked
// for rather than what a range holds.
func (p *Peer) ScanRange(
	ctx context.Context,
	r rangemap.Range,
	bucket, prefix, after string,
	limit int,
	fn func(metastore.Entry) error,
) error {
	resp, err := p.call(ctx, transport.ShardRequest{
		Op:     transport.ShardOpScan,
		Range:  &r,
		Bucket: bucket,
		Prefix: prefix,
		After:  after,
		Limit:  limit,
	})
	if err != nil {
		return err
	}

	for _, e := range resp.Entries {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "scan index")
		}

		if err := fn(e); err != nil {
			return err
		}
	}

	return nil
}

// SetVerified implements Backend.
func (p *Peer) SetVerified(ctx context.Context, records []metastore.Verification) error {
	if len(records) == 0 {
		return nil
	}

	_, err := p.call(ctx, transport.ShardRequest{Op: transport.ShardOpVerified, Records: records})

	return err
}

// Coverage implements Backend.
func (p *Peer) Coverage(ctx context.Context) (metastore.Coverage, error) {
	resp, err := p.call(ctx, transport.ShardRequest{Op: transport.ShardOpCoverage})
	if err != nil {
		return metastore.Coverage{}, err
	}

	if resp.Coverage == nil {
		return metastore.Coverage{}, nil
	}

	return *resp.Coverage, nil
}

// Measure implements Backend.
func (p *Peer) Measure(ctx context.Context, r rangemap.Range) (Measurement, error) {
	resp, err := p.call(ctx, transport.ShardRequest{Op: transport.ShardOpMeasure, Range: &r})
	if err != nil {
		return Measurement{}, err
	}

	return Measurement{Bytes: resp.Bytes, SplitAt: resp.SplitAt}, nil
}

// Reset implements Backend.
func (p *Peer) Reset(ctx context.Context) error {
	_, err := p.call(ctx, transport.ShardRequest{Op: transport.ShardOpReset})

	return err
}
