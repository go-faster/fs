package clusterstore

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Object checksums are MD5 by protocol.
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/placement"
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

	// sweepGrace protects unattributed generations (no committed sidecar
	// record names them — possibly another node's write mid-commit) from the
	// sweep: they are deleted only when still present a grace period after
	// first sighted.
	sweepGrace time.Duration

	// strayMu guards strays: objectRef → generation → first sighting.
	strayMu sync.Mutex
	strays  map[string]map[string]time.Time
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
	// SweepGrace is how long an unattributed generation (fragments no
	// committed record names — possibly another node's write mid-commit) is
	// left alone before the sweep may delete it. Defaults to
	// DefaultSweepGrace; only tests should lower it.
	SweepGrace time.Duration
}

// DefaultSweepGrace is the default protection window for unattributed
// generations — far beyond any write's commit latency.
const DefaultSweepGrace = 10 * time.Minute

// NewRepairer builds a repair worker over the coordinator.
func NewRepairer(cfg RepairerConfig) (*Repairer, error) {
	if cfg.Coordinator == nil {
		return nil, errors.New("clusterstore: repairer needs a Coordinator")
	}

	onErr := cfg.OnError
	if onErr == nil {
		onErr = func(string, string, error) {}
	}

	grace := cfg.SweepGrace
	if grace <= 0 {
		grace = DefaultSweepGrace
	}

	return &Repairer{
		coord:      cfg.Coordinator,
		self:       cfg.Self,
		verify:     cfg.Verify,
		onErr:      onErr,
		sweepGrace: grace,
		strays:     make(map[string]map[string]time.Time),
	}, nil
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
	// Converted counts objects rewritten to their bucket's current scheme.
	Converted int
	// ECUnverified is set when the EC parity/data consistency check failed:
	// without per-shard digests no victim can be identified, so nothing is
	// rebuilt and the object needs attention.
	ECUnverified bool
}

// Changed reports whether the pass modified anything.
func (r *RepairReport) Changed() bool {
	return r.RebuiltFragments > 0 || r.RewrittenSidecars > 0 || r.DeletedStale > 0 || r.Converted > 0
}

// add folds another report in.
func (r *RepairReport) add(o *RepairReport) {
	r.RebuiltFragments += o.RebuiltFragments
	r.RewrittenSidecars += o.RewrittenSidecars
	r.DeletedStale += o.DeletedStale
	r.CorruptReplicas += o.CorruptReplicas
	r.Converted += o.Converted
	r.ECUnverified = r.ECUnverified || o.ECUnverified
}

// localFragments describes one object's fragments found on a local disk by
// the scrubber: extra rebuild sources (and sweep targets) beyond what the
// remembered epochs predict — they keep relocation working even after a
// restart lost the epoch memory.
type localFragments struct {
	disk cluster.DiskID
	// gens maps generation → fragment indexes present locally.
	gens map[string][]int
}

// RepairObject restores one object to full protection at the current epoch's
// placement. It holds the object's async-work slot exclusively, so writes to
// the key wait for the repair (and the repair never races a pending
// remainder). Returns ErrNotFound when no committed sidecar is reachable and
// ErrUnrecoverable when too few fragments survive to rebuild.
func (r *Repairer) RepairObject(ctx context.Context, bucket, key string) (*RepairReport, error) {
	return r.repair(ctx, bucket, key, nil, nil)
}

// newerRecordError aborts a repair pass: a target holds a sidecar record
// superseding the pass's authoritative one — a concurrent overwrite from
// another node committed mid-pass. The pass must restart with the newer
// record; downgrading it (or sweeping its fragments) would destroy an
// acknowledged write.
type newerRecordError struct {
	sc *Sidecar
}

func (e *newerRecordError) Error() string {
	return "concurrent overwrite: generation " + e.sc.Generation + " supersedes the repair view"
}

// repair is RepairObject plus scrub-provided hints: a locally-read sidecar
// and locally-present fragments (relocation sources).
func (r *Repairer) repair(ctx context.Context, bucket, key string, hint *Sidecar, local *localFragments) (*RepairReport, error) {
	release := r.coord.exclusiveKey(bucket, key)
	defer release()

	sc, known, err := r.authoritativeSidecar(ctx, bucket, key, hint)
	if err != nil {
		return nil, err
	}

	report := &RepairReport{}

	// Bounded retries: a concurrent overwrite (from another node — the
	// exclusive slot is process-local) restarts the pass with the newer
	// record. A key hot enough to outrun the retries converges on a later
	// scrub.
	for range 3 {
		err := r.repairPass(ctx, sc, known, local, report)

		var newer *newerRecordError
		if errors.As(err, &newer) {
			known[sc.Generation] = struct{}{} // The old record is now positively superseded.
			sc = newer.sc

			continue
		}

		return report, err
	}

	return report, errors.Errorf("%s/%s: concurrent overwrites outran repair; next pass converges", bucket, key)
}

// repairPass runs one repair attempt against a fixed authoritative record.
// known holds every generation positively attributed to a committed sidecar
// record — the only generations the sweep may delete.
func (r *Repairer) repairPass(ctx context.Context, sc *Sidecar, known map[string]struct{}, local *localFragments, report *RepairReport) error {
	topo := r.coord.topo.Topology()

	s, plan, peers, err := r.coord.planFor(topo, sc)
	if err != nil {
		return err
	}

	// Survey each target: sidecar state, committed fragment state, strays.
	missing, err := r.survey(ctx, sc, plan, peers, report)
	if err != nil {
		return err
	}

	if r.verify {
		r.verifyReplicas(ctx, sc, s, plan, peers, missing, report)
	}

	// Old-placement copies first: after a topology change the fragments are
	// byte-identical, just on the wrong targets — a straight copy beats a
	// scheme rebuild.
	sources := r.fragmentSources(s, sc, local)
	r.restoreFromCopies(ctx, sc, plan, peers, missing, sources, report)

	// Last resort before declaring the object unrecoverable: hunt the whole
	// cluster for same-index copies the epoch memory does not predict (e.g.
	// old-placement fragments after a restart lost the memory).
	if sc.Size > 0 && !recoverable(s, missing) {
		r.huntAndRestore(ctx, sc, plan, peers, missing, report)
	}

	if err := r.rebuild(ctx, sc, s, plan, peers, missing, report); err != nil {
		return err
	}

	if r.verify && s.Kind == scheme.EC && len(missing) == 0 {
		r.verifyEC(ctx, sc, s, plan, peers, report)
	}

	// With the current placement fully healthy, retire old-placement copies
	// (copy → verify → delete: an object is never below its protection level
	// mid-move).
	if err := r.relocationSweep(ctx, sc, known, plan, peers, sources, local, report); err != nil {
		return err
	}

	// Scheme conversion (ROADMAP Phase 8): once the object is fully healthy
	// at its recorded scheme, rewrite it under the bucket's current scheme.
	// Requiring health first means the conversion source is complete and the
	// object is never below the stronger of the two guarantees mid-convert.
	if len(missing) == 0 && !report.ECUnverified {
		if err := r.maybeConvert(ctx, sc, s, report); err != nil {
			return err
		}
	}

	return nil
}

// fragmentSources maps fragment index → old-placement copies to restore from:
// previous-epoch targets plus scrub-discovered local fragments.
func (r *Repairer) fragmentSources(s scheme.Scheme, sc *Sidecar, local *localFragments) map[int][]candidateTarget {
	sources := make(map[int][]candidateTarget)

	plans := r.coord.epochPlans(s, sc.Bucket, sc.Key, sc.Size)
	for _, ep := range plans[1:] { // plans[0] is the current epoch (the destination).
		for _, item := range ep.plan {
			sources[item.Index] = append(sources[item.Index], candidateTarget{target: item.Target, topo: ep.topo})
		}
	}

	if local != nil {
		topo := r.coord.topo.Topology()
		for _, idx := range local.gens[sc.Generation] {
			sources[idx] = append(sources[idx], candidateTarget{
				target: placementTarget(r.self, local.disk),
				topo:   topo,
			})
		}
	}

	return sources
}

// recoverable reports whether the scheme can rebuild the missing set without
// further sources.
func recoverable(s scheme.Scheme, missing map[int]struct{}) bool {
	if s.Kind == scheme.EC {
		return s.K+s.M-len(missing) >= s.K
	}

	for i := range s.FullReplicas() {
		if _, gone := missing[i]; !gone {
			return true
		}
	}

	return false
}

// huntAndRestore scans every disk in the current topology for byte-identical
// copies of still-missing fragments and copies them home. Expensive (a stat
// per disk per fragment) and only used when the object would otherwise be
// unrecoverable.
func (r *Repairer) huntAndRestore(ctx context.Context, sc *Sidecar, plan []fragment.Item, peers []Peer, missing map[int]struct{}, report *RepairReport) {
	topo := r.coord.topo.Topology()

	for idx := range missing {
		name := fragmentName(sc.Bucket, sc.Key, sc.Generation, idx)

	hunt:
		for ni := range topo.Nodes {
			node := &topo.Nodes[ni]

			p, err := r.coord.dial(topo, node.ID)
			if err != nil {
				continue
			}

			for _, disk := range node.Disks {
				if node.ID == plan[idx].Target.Node && disk.ID == plan[idx].Target.Disk {
					continue // The missing destination itself.
				}

				rc, size, err := p.Get(ctx, disk.ID, name)
				if err != nil {
					continue
				}

				if size != plan[idx].Size {
					_ = rc.Close()
					continue
				}

				err = peers[idx].Put(ctx, plan[idx].Target.Disk, name, size, rc)
				_ = rc.Close()

				if err != nil {
					continue
				}

				delete(missing, idx)

				report.RebuiltFragments++

				break hunt
			}
		}
	}
}

// restoreFromCopies fills missing fragments by streaming byte-identical
// copies from old-placement sources.
func (r *Repairer) restoreFromCopies(ctx context.Context, sc *Sidecar, plan []fragment.Item, peers []Peer, missing map[int]struct{}, sources map[int][]candidateTarget, report *RepairReport) {
	for idx := range missing {
		name := fragmentName(sc.Bucket, sc.Key, sc.Generation, idx)

		for _, src := range sources[idx] {
			if targetRef(src.target) == targetRef(plan[idx].Target) {
				continue // The missing destination itself.
			}

			p, err := r.coord.dial(src.topo, src.target.Node)
			if err != nil {
				continue
			}

			rc, size, err := p.Get(ctx, src.target.Disk, name)
			if err != nil {
				continue
			}

			if size != plan[idx].Size {
				_ = rc.Close()
				continue
			}

			err = peers[idx].Put(ctx, plan[idx].Target.Disk, name, size, rc)
			_ = rc.Close()

			if err != nil {
				continue
			}

			delete(missing, idx)

			report.RebuiltFragments++

			break
		}
	}
}

// relocationSweep retires everything that should no longer exist — stale
// generations, misplaced committed-generation copies, whole old-placement
// targets — but ONLY once every current-epoch fragment is verifiably in
// place. Deletion is strictly copy → verify → delete: an object is never
// taken below its protection level mid-move, and an unhealthy object is
// never swept at all.
//
// Two more guards protect concurrent overwrites from other nodes:
//   - every target's sidecar is re-read right before its names are deleted,
//     and a superseding record aborts the pass (newerRecordError) — the
//     just-committed generation must not be swept as a stray;
//   - only positively attributed generations are deleted: the authoritative
//     one (outside its keep-set) and generations from committed-then-
//     superseded records (known). An unknown generation may be another
//     node's write racing between its fragment writes and its commit; it is
//     left for a later pass (once its record commits it becomes known, and
//     refused-write garbage is the mtime-sweep follow-up).
func (r *Repairer) relocationSweep(ctx context.Context, sc *Sidecar, known map[string]struct{}, plan []fragment.Item, peers []Peer, sources map[int][]candidateTarget, local *localFragments, report *RepairReport) error {
	// Cutover gate: every current-epoch fragment must be durably present.
	for i := range plan {
		name := fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[i].Index)
		if size, err := peers[i].Stat(ctx, plan[i].Target.Disk, name); err != nil || size != plan[i].Size {
			return nil
		}
	}

	metaName := sidecarName(sc.Bucket, sc.Key)
	base := objectBase(sc.Bucket, sc.Key) + "/"

	// What each current-placement target may keep.
	keep := make(map[diskRef]map[string]struct{}, len(plan))

	for i := range plan {
		ref := targetRef(plan[i].Target)
		keep[ref] = map[string]struct{}{
			metaName: {},
			fragmentName(sc.Bucket, sc.Key, sc.Generation, plan[i].Index): {},
		}
	}

	// Sweep set: current targets (down to their keep-set) plus every known
	// old-placement location (down to nothing).
	targets := make(map[diskRef]candidateTarget, len(plan))

	topo := r.coord.topo.Topology()
	for i := range plan {
		targets[targetRef(plan[i].Target)] = candidateTarget{target: plan[i].Target, topo: topo}
	}

	for _, cands := range sources {
		for _, ct := range cands {
			if _, ok := targets[targetRef(ct.target)]; !ok {
				targets[targetRef(ct.target)] = ct
			}
		}
	}

	if local != nil {
		ct := candidateTarget{target: placementTarget(r.self, local.disk), topo: topo}
		if _, ok := targets[targetRef(ct.target)]; !ok {
			targets[targetRef(ct.target)] = ct
		}
	}

	for ref, ct := range targets {
		p, err := r.coord.dial(ct.topo, ct.target.Node)
		if err != nil {
			continue // Unreachable target: retired by a later pass.
		}

		// Overwrite fence: a record newer than the pass's view aborts.
		if cur, err := readSidecarFrom(ctx, p, ct.target.Disk, metaName); err == nil && cur != nil && cur.Generation != sc.Generation && cur.Supersedes(sc) {
			return &newerRecordError{sc: cur}
		}

		names, err := p.List(ctx, ct.target.Disk, base)
		if err != nil {
			continue
		}

		for _, n := range names {
			if _, ok := keep[ref][n]; ok {
				continue
			}

			if gen, _, isFragment := parseFragmentFile(n[strings.LastIndex(n, "/")+1:]); isFragment {
				if _, attributed := known[gen]; !attributed && gen != sc.Generation && !r.strayExpired(sc.Bucket, sc.Key, gen) {
					continue // Possibly a racing write's fragment; not ours to delete yet.
				}
			}

			if err := p.Delete(ctx, ct.target.Disk, n); err == nil {
				report.DeletedStale++
			}
		}
	}

	r.forgetStrays(sc.Bucket, sc.Key, known, sc.Generation)

	return nil
}

// strayExpired tracks an unattributed generation's first sighting and reports
// whether it has outlived the sweep grace: garbage from refused writes is
// reclaimed on a later pass, while a racing write commits (and becomes
// attributed) long before its grace expires.
func (r *Repairer) strayExpired(bucket, key, gen string) bool {
	ref := objectRef(bucket, key)
	now := time.Now()

	r.strayMu.Lock()
	defer r.strayMu.Unlock()

	gens := r.strays[ref]
	if gens == nil {
		gens = make(map[string]time.Time)
		r.strays[ref] = gens
	}

	first, ok := gens[gen]
	if !ok {
		gens[gen] = now
		return false
	}

	return now.Sub(first) >= r.sweepGrace
}

// forgetStrays drops sighting state for generations that are now attributed
// (their record committed) — and, when nothing unattributed remains, the
// object's entry.
func (r *Repairer) forgetStrays(bucket, key string, known map[string]struct{}, current string) {
	ref := objectRef(bucket, key)

	r.strayMu.Lock()
	defer r.strayMu.Unlock()

	gens := r.strays[ref]

	for gen := range gens {
		if _, attributed := known[gen]; attributed || gen == current {
			delete(gens, gen)
		}
	}

	if len(gens) == 0 {
		delete(r.strays, ref)
	}
}

// authoritativeSidecar gathers every readable sidecar replica for the object
// — across all remembered topology epochs, plus the scrubber's locally-read
// hint — and returns the newest (same ordering as list-merge: Seq, Modified,
// then generation), along with the set of every generation seen in a
// committed record. Superseded generations in that set are the only ones the
// sweep may positively retire.
func (r *Repairer) authoritativeSidecar(ctx context.Context, bucket, key string, hint *Sidecar) (*Sidecar, map[string]struct{}, error) {
	name := sidecarName(bucket, key)

	var (
		best    = hint
		lastErr error
	)

	known := make(map[string]struct{})
	if hint != nil {
		known[hint.Generation] = struct{}{}
	}

	for _, ct := range r.coord.allSidecarCandidates(bucket, key) {
		p, err := r.coord.dial(ct.topo, ct.target.Node)
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
			lastErr = err
			continue
		}

		sc, err := decodeSidecar(data)
		if err != nil {
			continue // A corrupt replica; others decide.
		}

		known[sc.Generation] = struct{}{}

		if best == nil || sc.Supersedes(best) {
			best = sc
		}
	}

	if best != nil {
		return best, known, nil
	}

	if lastErr != nil {
		return nil, nil, lastErr
	}

	return nil, nil, errors.Wrapf(ErrNotFound, "%s/%s", bucket, key)
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

		// Sidecar replica present and current? Never overwrite a SUPERSEDING
		// record — that would downgrade a write that committed after this
		// pass's view was gathered; restart with the newer record instead.
		cur, err := readSidecarFrom(ctx, peers[i], disk, metaName)
		if err != nil {
			return nil, errors.Wrapf(err, "read sidecar on %s/%s", plan[i].Target.Node, disk)
		}

		if cur != nil && cur.Generation != sc.Generation && cur.Supersedes(sc) {
			return nil, &newerRecordError{sc: cur}
		}

		if cur == nil || cur.Generation != sc.Generation {
			if err := putBytes(ctx, peers[i], disk, metaName, data); err != nil {
				return nil, errors.Wrapf(err, "rewrite sidecar on %s/%s", plan[i].Target.Node, disk)
			}

			report.RewrittenSidecars++
		}

		// Committed fragment present with the planned size?
		if size, err := peers[i].Stat(ctx, disk, fragName); err != nil || size != plan[i].Size {
			missing[plan[i].Index] = struct{}{}
		}

		// NB: no deletions here. Stray names may be relocation sources (a
		// committed-generation fragment under another epoch's index); all
		// sweeping happens at the end of the pass, gated on the object being
		// verifiably healthy at the current placement.
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

	// Collect each namespace's locally-present fragment indexes by
	// generation — relocation sources for repair.
	frags := make(map[string]map[string][]int)

	for _, n := range names {
		dir, file := n[:strings.LastIndex(n, "/")], n[strings.LastIndex(n, "/")+1:]

		gen, idx, ok := parseFragmentFile(file)
		if !ok {
			continue
		}

		if frags[dir] == nil {
			frags[dir] = make(map[string][]int)
		}

		frags[dir][gen] = append(frags[dir][gen], idx)
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

		rep, err := r.repair(ctx, sc.Bucket, sc.Key, sc, &localFragments{disk: disk, gens: frags[dir]})
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

// parseFragmentFile splits a fragment file name "<generation>.f<index>".
func parseFragmentFile(file string) (gen string, idx int, ok bool) {
	gen, tail, found := strings.Cut(file, ".f")
	if !found || gen == "" {
		return "", 0, false
	}

	n, err := strconv.Atoi(tail)
	if err != nil || n < 0 {
		return "", 0, false
	}

	return gen, n, true
}

// placementTarget builds a placement target for a local (node, disk) pair.
func placementTarget(node cluster.NodeID, disk cluster.DiskID) placement.Target {
	return placement.Target{Node: node, Disk: disk}
}
