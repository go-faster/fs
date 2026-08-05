package etcd

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/cluster/metastore"
)

// planeStateKey holds the cluster-wide ready/building flag for the metadata
// plane.
//
// One key, not one per node. A plane that is being rebuilt is being rebuilt for
// everyone: a node that believed its own share was ready would serve listings
// from a partitioning whose other ranges hold nothing, and a listing missing
// keys is a wrong answer where the slower sidecar walk is a right one.
func (c Config) planeStateKey() string { return c.Prefix + "/metaplane/state" }

// PlaneState is the cluster-wide ready/building flag, watched rather than
// polled.
//
// # Why a watch, when routing deliberately has none
//
// The range map is 40,000 keys changing on every split, watched from ~650
// nodes; that fan-out is what the whole lazy-routing design exists to avoid.
// This is one key that changes when a rebuild starts and when it ends. The two
// are not the same problem, and the reason the map is not watched does not
// apply here.
//
// It has to be one or the other, because a listing consults this flag: reading
// etcd per LIST would put a control-plane round trip on the request path of the
// operation this project exists to make fast.
//
// # A broken watch reports building
//
// Not the last value seen. The two directions of staleness are not symmetric:
// believing the plane is building when it is ready costs a slower answer that
// is still correct, while believing it is ready when a rebuild has started
// serves a listing missing keys, which is simply wrong.
//
// The cost is real — an etcd hiccup drops every listing in the cluster onto the
// sidecar walk until the watch is back. That is the behavior this plane
// replaced, so it is degraded rather than broken, and it is the only direction
// worth failing in.
type PlaneState struct {
	client *clientv3.Client
	cfg    Config

	// cur is the last state seen, read lock-free on the listing path.
	cur atomic.Int32
	// cause is why it is building, parsed from the same value on the same
	// update. Separate from cur so the listing path stays one atomic load: a
	// listing asks whether the plane is usable and never asks why.
	cause atomic.Int32
	// watching is false while the watch is broken or resyncing, which is what
	// makes State report building without changing cur.
	watching atomic.Bool

	cancel context.CancelFunc
	done   sync.WaitGroup

	// OnError observes background watch failures; the state keeps serving and
	// retries. Set before Watch; may be nil.
	OnError func(err error)
}

// Encoded states. Kept as small integers rather than strings so the read on the
// listing path is a single atomic load.
const (
	stateBuilding int32 = iota
	stateReady
)

// NewPlaneState loads the flag and starts watching it. The returned state
// answers immediately.
func NewPlaneState(ctx context.Context, client *clientv3.Client, cfg Config) (*PlaneState, error) {
	cfg = cfg.withDefaults()

	p := &PlaneState{client: client, cfg: cfg}

	rev, err := p.load(ctx)
	if err != nil {
		return nil, err
	}

	p.watching.Store(true)

	// The watch outlives ctx, which is the caller's startup context; Close ends
	// it. A watch bound to startup would stop the moment the node finished
	// starting, leaving every listing on the sidecar walk forever.
	watchCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	p.done.Add(1)

	go func() {
		defer p.done.Done()

		p.watch(watchCtx, rev+1)
	}()

	return p, nil
}

// State implements the plane's readiness interface.
//
// Never fails: a plane whose flag cannot be read is one whose listings must
// fall back, and that is a state rather than an error.
func (p *PlaneState) State(context.Context) (metastore.State, error) {
	if !p.watching.Load() {
		return metastore.StateBuilding, nil
	}

	if p.cur.Load() == stateReady {
		return metastore.StateReady, nil
	}

	return metastore.StateBuilding, nil
}

// Status is the flag and, when it is building, why.
//
// A broken watch reports building with no cause, not the last cause seen. The
// flag failing toward building is what keeps listings correct; carrying a
// remembered *reason* through that would let a rebuild start on the strength of
// something nobody can currently read.
func (p *PlaneState) Status(ctx context.Context) (metastore.Build, error) {
	state, err := p.State(ctx)
	if err != nil || state == metastore.StateReady {
		return metastore.Build{State: state}, err
	}

	if !p.watching.Load() {
		return metastore.Building(metastore.CauseUnspecified), nil
	}

	return metastore.Building(metastore.BuildCause(p.cause.Load())), nil //nolint:gosec // Stored from a BuildCause below.
}

// readyMarker is what a ready plane writes.
//
// A word rather than the numeric State, because this key is one an operator
// reads with etcdctl while deciding whether a cluster is healthy, and "1" is
// not an answer to that question.
const readyMarker = "ready"

// Set publishes the flag.
//
// The local value is not updated here; it arrives through the watch, like
// everyone else's. There is one path into the cached state rather than two, so
// there is nothing to keep consistent — not a correctness requirement, since a
// writer briefly ahead of its own watch would be right either way, but one path
// is easier to reason about than two that must agree.
func (p *PlaneState) Set(ctx context.Context, build metastore.Build) error {
	value := readyMarker

	if build.State != metastore.StateReady {
		// "building:orphaned" rather than a second key, so the flag and the
		// reason for it can never be read a revision apart — and so an older
		// node, which compares against the ready marker and calls everything
		// else building, reads a newer cluster correctly.
		value = "building:" + build.Cause.String()
	}

	if _, err := p.client.Put(ctx, p.cfg.planeStateKey(), value); err != nil {
		return errors.Wrap(err, "publish metadata plane state")
	}

	return nil
}

// Close stops the watch.
func (p *PlaneState) Close() error {
	if p.cancel != nil {
		p.cancel()
	}

	p.done.Wait()

	return nil
}

// load reads the flag and returns the revision it was read at.
func (p *PlaneState) load(ctx context.Context) (int64, error) {
	resp, err := p.client.Get(ctx, p.cfg.planeStateKey())
	if err != nil {
		return 0, errors.Wrap(err, "load metadata plane state")
	}

	// An absent key is building: a plane nobody has ever marked ready has not
	// been built, and a fresh cluster must walk its sidecars until it has.
	value := ""
	if len(resp.Kvs) > 0 {
		value = string(resp.Kvs[0].Value)
	}

	p.store(value)

	return resp.Header.Revision, nil
}

// store folds a raw value into the cached state. Anything that is not exactly
// the ready marker is building, so a corrupt or partially written value fails
// toward the correct-and-slow answer.
func (p *PlaneState) store(value string) {
	if value == readyMarker {
		// The cause is left as it was, and never read: Status answers a ready
		// plane without consulting it, because a store that is usable has no
		// reason to be unusable. Clearing it here would be a second place that
		// has to agree with that, for no reader.
		p.cur.Store(stateReady)

		return
	}

	p.cur.Store(stateBuilding)
	p.cause.Store(int32(causeOf(value)))
}

// causeOf reads the reason out of a building marker.
//
// Anything it does not recognize is unspecified, which is the cautious answer:
// a rebuild is hours of I/O, and a value written by a version that named its
// causes differently is not grounds to start one.
func causeOf(value string) metastore.BuildCause {
	name, ok := strings.CutPrefix(value, "building:")
	if !ok {
		return metastore.CauseUnspecified
	}

	switch name {
	case metastore.CauseOrphaned.String():
		return metastore.CauseOrphaned
	case metastore.CauseNeverBuilt.String():
		return metastore.CauseNeverBuilt
	default:
		return metastore.CauseUnspecified
	}
}

// watch applies flag changes from rev onward, re-establishing the watch with a
// fresh load whenever it breaks.
func (p *PlaneState) watch(ctx context.Context, rev int64) {
	for {
		ch := p.client.Watch(ctx, p.cfg.planeStateKey(), clientv3.WithRev(rev))

		for resp := range ch {
			if err := resp.Err(); err != nil {
				p.watching.Store(false)
				p.reportErr(errors.Wrap(err, "metadata plane state watch"))

				break
			}

			for _, ev := range resp.Events {
				// A delete arrives with an empty value, which store already
				// reads as building — the key going away is the same statement
				// as it never having existed: nobody vouches for this plane.
				p.store(string(ev.Kv.Value))
			}

			rev = resp.Header.Revision + 1
		}

		if contextDone(ctx) {
			return
		}

		// The watch is gone for whatever reason the channel closed, not only
		// for the ones that arrived as an error.
		p.watching.Store(false)

		select {
		case <-ctx.Done():
			return
		case <-time.After(rewatchBackoff):
		}

		loaded, err := p.load(ctx)
		if err != nil {
			p.reportErr(err)

			continue
		}

		rev = loaded + 1

		p.watching.Store(true)
	}
}

func (p *PlaneState) reportErr(err error) {
	if p.OnError != nil {
		p.OnError(err)
	}
}
