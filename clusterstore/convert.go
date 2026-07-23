package clusterstore

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Object checksums are MD5 by protocol.
	"encoding/hex"
	"io"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// maybeConvert rewrites a healthy object under its bucket's current scheme
// when the recorded one differs (RF2.5 ↔ RF3 ↔ EC, ROADMAP Phase 8). Called
// at the end of a repair pass, under the object's exclusive slot.
func (r *Repairer) maybeConvert(ctx context.Context, sc *Sidecar, cur scheme.Scheme, report *RepairReport) error {
	target, err := r.coord.freshBucketScheme(ctx, sc.Bucket)
	if err != nil {
		return nil //nolint:nilerr // Bucket record unreachable: convert on a later pass, never on a guess.
	}

	if target.String() == sc.Scheme {
		return nil
	}

	return r.convert(ctx, sc, cur, target, report)
}

// convert re-encodes the object under target: every new-scheme fragment and
// sidecar replica is written and verified synchronously, and only then is the
// old generation retired — the object is never below the stronger of the two
// guarantees mid-convert. The rewrite keeps the object's identity (ETag,
// Modified, metadata); only Seq advances, so the converted record supersedes
// the old one while any racing user write (same Seq bump, later Modified)
// still wins.
func (r *Repairer) convert(ctx context.Context, sc *Sidecar, cur, target scheme.Scheme, report *RepairReport) error {
	topo := r.coord.topo.Topology()

	plan, err := fragment.Plan(topo, target, placement.ObjectKey(sc.Bucket, sc.Key), sc.Size)
	if err != nil {
		return errors.Wrapf(err, "plan conversion to %s", target)
	}

	peers, err := r.coord.dialPlan(topo, plan)
	if err != nil {
		return err
	}

	gen, err := newGeneration()
	if err != nil {
		return err
	}

	// Content source: the object at its recorded scheme (health was just
	// verified by the repair pass).
	body, err := r.openContent(ctx, sc, cur)
	if err != nil {
		return errors.Wrap(err, "open conversion source")
	}

	defer func() { _ = body.Close() }()

	sink := func(item fragment.Item) (io.WriteCloser, error) {
		name := fragmentName(sc.Bucket, sc.Key, gen, item.Index)

		return newPutSink(ctx, peers[item.Index], item.Target.Disk, name, item.Size), nil
	}
	reopen := func(item fragment.Item) (io.ReadCloser, error) {
		rc, _, err := peers[item.Index].Get(ctx, item.Target.Disk, fragmentName(sc.Bucket, sc.Key, gen, item.Index))

		return rc, err
	}

	hasher := md5.New() //nolint:gosec // Content checksum, not a security primitive.

	// Every fragment — data, parity, trailing replica — synchronously: there
	// is no async quorum shortcut on a conversion.
	if err := fragment.EncodeStream(plan, target, sc.Size, io.TeeReader(body, hasher), sink, reopen); err != nil {
		r.coord.discardGeneration(ctx, plan, peers, sc.Bucket, sc.Key, gen)

		return errors.Wrapf(err, "write %s fragments", target)
	}

	// The re-read content must be the committed content; on mismatch nothing
	// is converted (verify-enabled scrub finds and repairs the corruption).
	if sum := hex.EncodeToString(hasher.Sum(nil)); sc.Checksum != "" && sum != sc.Checksum {
		r.coord.discardGeneration(ctx, plan, peers, sc.Bucket, sc.Key, gen)

		return errors.Errorf("conversion source checksum mismatch: %s != %s", sum, sc.Checksum)
	}

	newSC := *sc
	newSC.Scheme = target.String()
	newSC.Generation = gen
	newSC.Seq = sc.Seq + 1

	// Commit to every target — the converted object starts at full
	// protection, not at quorum.
	if err := r.coord.commitSidecar(ctx, plan, peers, &newSC, sc, len(plan)); err != nil {
		r.coord.discardGeneration(ctx, plan, peers, sc.Bucket, sc.Key, gen)

		return err
	}

	report.Converted++

	// Only now retire the old generation: its fragments across every
	// remembered epoch, and stale sidecars on targets outside the new
	// placement. Best-effort — leftovers are swept by later passes.
	if err := r.coord.cleanupGeneration(ctx, topo, plan, sc); err != nil {
		r.onErr(sc.Bucket, sc.Key, errors.Wrap(err, "retire pre-conversion generation"))
	}

	return nil
}

// openContent streams the object's payload at its recorded scheme, spanning
// remembered epochs like the read path.
func (r *Repairer) openContent(ctx context.Context, sc *Sidecar, cur scheme.Scheme) (io.ReadCloser, error) {
	if sc.Size == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	plans := r.coord.epochPlans(cur, sc.Bucket, sc.Key, sc.Size)
	if len(plans) == 0 {
		return nil, errors.Wrapf(ErrUnrecoverable, "no epoch can place %s/%s", sc.Bucket, sc.Key)
	}

	if cur.Kind == scheme.EC {
		return r.coord.openEC(ctx, plans, sc, cur), nil
	}

	return r.coord.openReplica(ctx, plans, sc, cur)
}
