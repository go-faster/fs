package clusterstore

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Object checksums are MD5 by protocol.
	"encoding/hex"
	"io"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// Repairer is the scheme-aware repair worker (ROADMAP Phase 8): it restores
// an object to its scheme's full protection level — rebuilding lost or
// corrupt fragments, completing missing sidecar replicas, and sweeping stale
// generations. The scrubber walks a node's local disks and feeds every object
// it finds through RepairObject, so a cluster of periodically-scrubbing nodes
// converges after node/disk loss or missed async remainders.
type Repairer struct {
	coord *Coordinator
	self  cluster.NodeID
	// verify re-reads replica payloads against the sidecar checksum during
	// repair, catching at-rest bit-rot (costs a full read per replica).
	verify bool
	onErr  func(bucket, key string, err error)
}

// RepairerConfig configures a Repairer.
type RepairerConfig struct {
	// Coordinator is the cluster data plane. Required.
	Coordinator *Coordinator
	// Self is this node's ID; Scrub walks its disks. Required for Scrub.
	Self cluster.NodeID
	// Verify enables checksum verification of replica fragments during
	// repair (recommended for scheduled scrubs).
	Verify bool
	// OnError observes per-object scrub failures. May be nil.
	OnError func(bucket, key string, err error)
}

// NewRepairer builds a repair worker over the coordinator.
func NewRepairer(cfg RepairerConfig) (*Repairer, error) {
	if cfg.Coordinator == nil {
		return nil, errors.New("clusterstore: repairer needs a Coordinator")
	}

	onErr := cfg.OnError
	if onErr == nil {
		onErr = func(string, string, error) {}
	}

	return &Repairer{coord: cfg.Coordinator, self: cfg.Self, verify: cfg.Verify, onErr: onErr}, nil
}

// RepairReport is what one RepairObject pass did.
type RepairReport struct {
	// RebuiltFragments counts fragments restored (missing, torn or corrupt).
	RebuiltFragments int
	// RewrittenSidecars counts targets whose sidecar was missing or stale and
	// was rewritten to the authoritative record.
	RewrittenSidecars int
	// DeletedStale counts swept names: superseded generations and orphaned
	// fragments on the object's targets.
	DeletedStale int
	// CorruptReplicas counts replicas whose payload failed checksum
	// verification (each is also rebuilt and counted in RebuiltFragments).
	CorruptReplicas int
	// ECUnverified is set when the EC parity/data consistency check failed:
	// without per-shard digests no victim can be identified, so nothing is
	// rebuilt and the object needs attention.
	ECUnverified bool
}

// Changed reports whether the pass modified anything.
func (r *RepairReport) Changed() bool {
	return r.RebuiltFragments > 0 || r.RewrittenSidecars > 0 || r.DeletedStale > 0
}

// add folds another report in.
func (r *RepairReport) add(o *RepairReport) {
	r.RebuiltFragments += o.RebuiltFragments
	r.RewrittenSidecars += o.RewrittenSidecars
	r.DeletedStale += o.DeletedStale
	r.CorruptReplicas += o.CorruptReplicas
	r.ECUnverified = r.ECUnverified || o.ECUnverified
}

// RepairObject restores one object to full protection. It holds the object's
// async-work slot exclusively, so writes to the key wait for the repair (and
// the repair never races a pending remainder). Returns ErrNotFound when no
// committed sidecar is reachable and ErrUnrecoverable when too few fragments
// survive to rebuild.
func (r *Repairer) RepairObject(ctx context.Context, bucket, key string) (*RepairReport, error) {
	release := r.coord.exclusiveKey(bucket, key)
	defer release()

	topo := r.coord.topo.Topology()

	sc, err := r.authoritativeSidecar(ctx, topo, bucket, key)
	if err != nil {
		return nil, err
	}

	s, plan, peers, err := r.coord.planFor(topo, sc)
	if err != nil {
		return nil, err
	}

	report := &RepairReport{}

	// Survey each target: sidecar state, committed fragment state, strays.
	missing, err := r.survey(ctx, sc, plan, peers, report)
	if err != nil {
		return nil, err
	}

	if r.verify {
		r.verifyReplicas(ctx, sc, s, plan, peers, missing, report)
	}

	if err := r.rebuild(ctx, sc, s, plan, peers, missing, report); err != nil {
		return report, err
	}

	if r.verify && s.Kind == scheme.EC && len(missing) == 0 {
		r.verifyEC(ctx, sc, s, plan, peers, report)
	}

	return report, nil
}

// authoritativeSidecar gathers every readable sidecar replica for the object
// and returns the newest (same ordering as list-merge: Modified, then
// generation).
func (r *Repairer) authoritativeSidecar(ctx context.Context, topo *cluster.Topology, bucket, key string) (*Sidecar, error) {
	name := sidecarName(bucket, key)

	var (
		best    *Sidecar
		lastErr error
	)

	for _, t := range r.coord.sidecarCandidates(topo, bucket, key) {
		p, err := r.coord.dial(topo, t.Node)
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
			lastErr = err
			continue
		}

		sc, err := decodeSidecar(data)
		if err != nil {
			continue // A corrupt replica; others decide.
		}

		if best == nil || sc.Modified.After(best.Modified) ||
			(sc.Modified.Equal(best.Modified) && sc.Generation > best.Generation) {
			best = sc
		}
	}

	if best != nil {
		return best, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, errors.Wrapf(ErrNotFound, "%s/%s", bucket, key)
}

// survey inspects every target: rewrites missing/stale sidecars, sweeps stray
// names, and returns the set of fragment indexes needing a rebuild.
func (r *Repairer) survey(ctx context.Context, sc *Sidecar, plan []fragment.Item, peers []Peer, report *RepairReport) (map[int]struct{}, error) {
	metaName := sidecarName(sc.Bucket, sc.Key)

	data, err := sc.encode()
	if err != nil {
		return nil, err
	}

	missing := make(map[int]struct{})

	for i := range plan {
		disk := plan[i].Target.Disk
		fragName := fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[i].Index)

		// Sidecar replica present and current?
		if cur, err := readSidecarFrom(ctx, peers[i], disk, metaName); err != nil || cur == nil || cur.Generation != sc.Generation {
			if err := putBytes(ctx, peers[i], disk, metaName, data); err != nil {
				return nil, errors.Wrapf(err, "rewrite sidecar on %s/%s", plan[i].Target.Node, disk)
			}

			report.RewrittenSidecars++
		}

		// Committed fragment present with the planned size?
		if size, err := peers[i].Stat(ctx, disk, fragName); err != nil || size != plan[i].Size {
			missing[plan[i].Index] = struct{}{}
		}

		// Sweep strays under the object's namespace on this target: anything
		// but the sidecar and this target's committed fragment.
		names, err := peers[i].List(ctx, disk, objectBase(sc.Bucket, sc.Key)+"/")
		if err != nil {
			continue // Listing is advisory; a down peer skips its sweep.
		}

		for _, n := range names {
			if n == metaName || n == fragName {
				continue
			}

			if err := peers[i].Delete(ctx, disk, n); err == nil {
				report.DeletedStale++
			}
		}
	}

	return missing, nil
}

// readSidecarFrom fetches and decodes one target's sidecar replica; (nil,
// nil) when absent or corrupt.
func readSidecarFrom(ctx context.Context, p Peer, disk cluster.DiskID, name string) (*Sidecar, error) {
	rc, _, err := p.Get(ctx, disk, name)
	if err != nil {
		if errors.Is(err, transport.ErrNotFound) {
			return nil, nil
		}

		return nil, err
	}

	data, err := io.ReadAll(rc)
	_ = rc.Close()

	if err != nil {
		return nil, err
	}

	sc, err := decodeSidecar(data)
	if err != nil {
		return nil, nil //nolint:nilerr // Corrupt replica reads as absent; it will be rewritten.
	}

	return sc, nil
}

// verifyReplicas re-reads present replica fragments and marks checksum
// mismatches for rebuild (deleting the corrupt payload first).
func (r *Repairer) verifyReplicas(ctx context.Context, sc *Sidecar, s scheme.Scheme, plan []fragment.Item, peers []Peer, missing map[int]struct{}, report *RepairReport) {
	if s.Kind == scheme.EC || sc.Size == 0 || sc.Checksum == "" {
		return
	}

	for i := range s.FullReplicas() {
		if _, gone := missing[i]; gone {
			continue
		}

		name := fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[i].Index)

		rc, _, err := peers[i].Get(ctx, plan[i].Target.Disk, name)
		if err != nil {
			missing[i] = struct{}{}
			continue
		}

		hasher := md5.New() //nolint:gosec // Content checksum.
		_, err = io.Copy(hasher, rc)
		_ = rc.Close()

		if err != nil || hex.EncodeToString(hasher.Sum(nil)) != sc.Checksum {
			// Bit-rot: drop the corrupt payload and rebuild from a healthy
			// replica.
			_ = peers[i].Delete(ctx, plan[i].Target.Disk, name)

			missing[i] = struct{}{}
			report.CorruptReplicas++
		}
	}
}

// rebuild restores every missing fragment, scheme-aware.
func (r *Repairer) rebuild(ctx context.Context, sc *Sidecar, s scheme.Scheme, plan []fragment.Item, peers []Peer, missing map[int]struct{}, report *RepairReport) error {
	if len(missing) == 0 {
		return nil
	}

	// An empty object's fragments are all empty; just materialize them.
	if sc.Size == 0 {
		for idx := range missing {
			name := fragmentName(sc.Bucket, sc.Key, sc.Generation, idx)
			if err := peers[idx].Put(ctx, plan[idx].Target.Disk, name, 0, bytes.NewReader(nil)); err != nil {
				return errors.Wrapf(err, "restore empty fragment %d", idx)
			}

			report.RebuiltFragments++
		}

		return nil
	}

	if s.Kind == scheme.EC {
		return r.rebuildEC(ctx, sc, s, plan, peers, missing, report)
	}

	return r.rebuildReplicas(ctx, sc, s, plan, peers, missing, report)
}

// rebuildReplicas restores RF-scheme fragments: full replicas stream from a
// surviving replica, the RF=2.5 parity is recomputed from the primary.
func (r *Repairer) rebuildReplicas(ctx context.Context, sc *Sidecar, s scheme.Scheme, plan []fragment.Item, peers []Peer, missing map[int]struct{}, report *RepairReport) error {
	src := -1

	for i := range s.FullReplicas() {
		if _, gone := missing[i]; !gone {
			src = i
			break
		}
	}

	if src < 0 {
		return errors.Wrapf(ErrUnrecoverable, "%s/%s: no healthy replica to rebuild from", sc.Bucket, sc.Key)
	}

	// Full replicas first (the parity pass reads plan[0]).
	for i := range s.FullReplicas() {
		if _, gone := missing[i]; !gone {
			continue
		}

		if err := r.copyFragment(ctx, sc, plan, peers, src, i); err != nil {
			return err
		}

		delete(missing, i)

		report.RebuiltFragments++
	}

	if _, gone := missing[2]; gone && s.Kind == scheme.RF25 {
		sink := func(item fragment.Item) (io.WriteCloser, error) {
			name := fragmentName(sc.Bucket, sc.Key, sc.Generation, item.Index)
			return newPutSink(ctx, peers[item.Index], item.Target.Disk, name, item.Size), nil
		}
		reopen := func(item fragment.Item) (io.ReadCloser, error) {
			rc, _, err := peers[item.Index].Get(ctx, item.Target.Disk, fragmentName(sc.Bucket, sc.Key, sc.Generation, item.Index))
			return rc, err
		}

		if err := fragment.EncodeParityStream(plan, s, sc.Size, sink, reopen); err != nil {
			return errors.Wrap(err, "recompute parity")
		}

		report.RebuiltFragments++
	}

	return nil
}

// copyFragment streams one fragment from a healthy target to a missing one.
func (r *Repairer) copyFragment(ctx context.Context, sc *Sidecar, plan []fragment.Item, peers []Peer, src, dst int) error {
	srcName := fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[src].Index)

	rc, size, err := peers[src].Get(ctx, plan[src].Target.Disk, srcName)
	if err != nil {
		return errors.Wrapf(err, "open source replica %d", src)
	}

	defer func() { _ = rc.Close() }()

	dstName := fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[dst].Index)
	if err := peers[dst].Put(ctx, plan[dst].Target.Disk, dstName, size, rc); err != nil {
		return errors.Wrapf(err, "restore replica %d", dst)
	}

	return nil
}

// rebuildEC reconstructs missing shards from any k survivors, streaming
// straight to their targets.
func (r *Repairer) rebuildEC(ctx context.Context, sc *Sidecar, s scheme.Scheme, plan []fragment.Item, peers []Peer, missing map[int]struct{}, report *RepairReport) error {
	total := s.K + s.M
	if total-len(missing) < s.K {
		return errors.Wrapf(ErrUnrecoverable, "%s/%s: %d shards left, need %d", sc.Bucket, sc.Key, total-len(missing), s.K)
	}

	codec, err := s.Codec()
	if err != nil {
		return err
	}

	valid := make([]io.Reader, total)
	fill := make([]io.Writer, total)

	var (
		readers []io.ReadCloser
		sinks   []io.WriteCloser
	)

	defer func() {
		for _, rc := range readers {
			_ = rc.Close()
		}
	}()

	for i := range total {
		name := fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[i].Index)

		if _, gone := missing[i]; gone {
			w := newPutSink(ctx, peers[i], plan[i].Target.Disk, name, plan[i].Size)
			fill[i] = w
			sinks = append(sinks, w)

			continue
		}

		rc, _, err := peers[i].Get(ctx, plan[i].Target.Disk, name)
		if err != nil {
			return errors.Wrapf(err, "open shard %d", i)
		}

		valid[i] = rc
		readers = append(readers, rc)
	}

	if err := codec.ReconstructStream(valid, fill); err != nil {
		for _, w := range sinks {
			_ = w.Close()
		}

		return errors.Wrap(err, "reconstruct shards")
	}

	for _, w := range sinks {
		if err := w.Close(); err != nil {
			return errors.Wrap(err, "commit rebuilt shard")
		}
	}

	report.RebuiltFragments += len(sinks)

	return nil
}

// verifyEC checks data/parity consistency across a complete shard set. A
// failure is only reportable: without per-shard digests the corrupt shard
// cannot be identified.
func (r *Repairer) verifyEC(ctx context.Context, sc *Sidecar, s scheme.Scheme, plan []fragment.Item, peers []Peer, report *RepairReport) {
	if sc.Size == 0 {
		return
	}

	codec, err := s.Codec()
	if err != nil {
		return
	}

	total := s.K + s.M
	shards := make([]io.Reader, total)
	closers := make([]io.ReadCloser, 0, total)

	defer func() {
		for _, rc := range closers {
			_ = rc.Close()
		}
	}()

	for i := range total {
		rc, _, err := peers[i].Get(ctx, plan[i].Target.Disk, fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[i].Index))
		if err != nil {
			return // A shard vanished mid-repair; the next scrub pass re-runs.
		}

		shards[i] = rc
		closers = append(closers, rc)
	}

	if ok, err := codec.VerifyStream(shards); err == nil && !ok {
		report.ECUnverified = true
	}
}

// ScrubReport summarizes one scrub pass over this node's disks.
type ScrubReport struct {
	// Objects is how many distinct objects were fed through repair.
	Objects int
	// Repaired counts objects where the pass changed anything.
	Repaired int
	// Failed counts objects whose repair errored (also reported to OnError).
	Failed int
	// UnknownDirs counts object namespaces on local disks with no readable
	// local sidecar — undecidable without cross-checking; left untouched.
	UnknownDirs int
	// Totals aggregates the per-object repair actions.
	Totals RepairReport
}

// Scrub walks this node's local disks and repairs every object found,
// cluster-wide: a missing remainder, a dead peer's fragment or a stale
// generation anywhere in the object's placement gets fixed, not just local
// state. Objects only reachable through other nodes' disks are covered by
// those nodes' scrubs.
func (r *Repairer) Scrub(ctx context.Context) (*ScrubReport, error) {
	topo := r.coord.topo.Topology()

	self, err := r.coord.dial(topo, r.self)
	if err != nil {
		return nil, errors.Wrap(err, "dial local node")
	}

	var disks []cluster.Disk

	for i := range topo.Nodes {
		if topo.Nodes[i].ID == r.self {
			disks = topo.Nodes[i].Disks
			break
		}
	}

	report := &ScrubReport{}
	seen := make(map[string]struct{})

	for _, disk := range disks {
		if err := r.scrubDisk(ctx, self, disk.ID, seen, report); err != nil {
			return report, err
		}
	}

	return report, nil
}

// scrubDisk feeds one disk's objects through repair.
func (r *Repairer) scrubDisk(ctx context.Context, self Peer, disk cluster.DiskID, seen map[string]struct{}, report *ScrubReport) error {
	names, err := self.List(ctx, disk, "obj/")
	if err != nil {
		return errors.Wrapf(err, "list disk %s", disk)
	}

	// Group names into object namespaces and find each one's local sidecar.
	metas := make(map[string]bool)

	for _, n := range names {
		dir := n[:strings.LastIndex(n, "/")]
		metas[dir] = metas[dir] || strings.HasSuffix(n, "/meta")
	}

	for dir, hasMeta := range metas {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !hasMeta {
			// Fragments with no local commit record: either a refused write's
			// garbage or a lost sidecar. Undecidable from names alone (they
			// are hashes); the object's other targets repair it, and a
			// mtime-based orphan sweep is a follow-up.
			report.UnknownDirs++
			continue
		}

		sc, err := readSidecarFrom(ctx, self, disk, dir+"/meta")
		if err != nil || sc == nil {
			report.UnknownDirs++
			continue
		}

		ref := objectRef(sc.Bucket, sc.Key)
		if _, done := seen[ref]; done {
			continue
		}

		seen[ref] = struct{}{}
		report.Objects++

		rep, err := r.RepairObject(ctx, sc.Bucket, sc.Key)
		if err != nil {
			report.Failed++

			r.onErr(sc.Bucket, sc.Key, err)

			continue
		}

		if rep.Changed() {
			report.Repaired++
		}

		report.Totals.add(rep)
	}

	return nil
}
