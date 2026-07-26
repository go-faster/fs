package diskstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// checkpointName is the per-disk index file, at the disk root. It is dot-named
// so the fragment walkers skip it: a plain file at the root would otherwise
// read as a fragment and make a drained disk look occupied forever.
const checkpointName = ".usage.json"

// checkpointVersion stamps the checkpoint format. A checkpoint written by a
// different version is discarded rather than guessed at — the cost is one
// rescan, and the alternative is counters nobody can explain.
const checkpointVersion = 1

// Occupancy is what one disk holds, as the node's own index reports it: an
// occupancy figure, not a capacity one. DiskUsage answers "how big is the
// filesystem" from statfs; this answers "how much of it is ours", which is the
// question a drain asks.
type Occupancy struct {
	// Fragments is how many fragments the disk holds.
	Fragments int64
	// Bytes is their total size — payload only, no filesystem overhead, so it
	// is smaller than the used space statfs reports.
	Bytes int64
	// Anchored reports whether a full scan has established these counters.
	// Until it has, they are zero and mean nothing: a node that restarted
	// uncleanly has to walk its disks before it can count them. Progress
	// readouts must say "scanning" rather than "empty".
	Anchored bool
	// ScannedAt is when the anchoring scan finished.
	ScannedAt time.Time
}

// Empty reports whether the index believes the disk holds nothing. It is only
// meaningful when Anchored.
func (u Occupancy) Empty() bool { return u.Anchored && u.Fragments == 0 }

// diskIndex tracks one disk's occupancy incrementally.
//
// It is a progress signal, never a safety gate. The counters are anchored by a
// full scan and then maintained by the write path, so they drift under exactly
// one condition: a fragment created or deleted while a scan is walking may land
// on the wrong side of the walker and be counted twice or not at all. The drift
// is bounded by the writes in that window and disappears at the next scan.
// Store.HasData stays the authority for "is this disk empty" — it is exact, and
// deleting a volume is not a decision to make on an estimate. See the drain
// notes in docs/SIZING.md.
type diskIndex struct {
	mu        sync.Mutex
	fragments int64
	bytes     int64
	anchored  bool
	scannedAt time.Time
}

// checkpoint is the on-disk form of an index.
type checkpoint struct {
	Version   int       `json:"version"`
	Fragments int64     `json:"fragments"`
	Bytes     int64     `json:"bytes"`
	ScannedAt time.Time `json:"scanned_at"`
	// Clean marks a checkpoint written by an orderly shutdown, which is the
	// only kind worth adopting. A checkpoint is written unclean at startup and
	// clean at Close, so a process that dies in between leaves counters that
	// say "rescan me" rather than counters that are quietly short by whatever
	// was written after the last flush.
	Clean bool `json:"clean"`
}

// newDiskIndex builds a disk's index, adopting a clean checkpoint if the last
// shutdown left one. Without it the index starts unanchored — counting nothing
// and saying so — until a scan runs.
func newDiskIndex(root string) *diskIndex {
	cp, ok := loadCheckpoint(root)
	if !ok {
		return &diskIndex{}
	}

	return &diskIndex{
		fragments: cp.Fragments,
		bytes:     cp.Bytes,
		anchored:  true,
		scannedAt: cp.ScannedAt,
	}
}

// usage returns the current counters.
func (d *diskIndex) usage() Occupancy {
	d.mu.Lock()
	defer d.mu.Unlock()

	return Occupancy{
		Fragments: d.fragments,
		Bytes:     d.bytes,
		Anchored:  d.anchored,
		ScannedAt: d.scannedAt,
	}
}

// added folds in a fragment that landed. prev is the size it replaced and
// existed says whether it replaced anything — a sidecar commit overwrites the
// same name on every write, so replacement is the common case, not the edge.
func (d *diskIndex) added(size, prev int64, existed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !existed {
		d.fragments++
	}

	d.bytes += size - prevBytes(prev, existed)
}

// removed folds in a deleted fragment.
func (d *diskIndex) removed(size int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.fragments--
	d.bytes -= size
}

// prevBytes is the byte count a replacement displaces.
func prevBytes(prev int64, existed bool) int64 {
	if !existed {
		return 0
	}

	return prev
}

// anchor installs a scan result. Deltas applied while the scan ran are kept:
// the scan is a floor, and the alternative — dropping them — loses every write
// that raced the walk instead of possibly double-counting a few.
func (d *diskIndex) anchor(fragments, bytes, deltaFragments, deltaBytes int64, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.fragments = fragments + (d.fragments - deltaFragments)
	d.bytes = bytes + (d.bytes - deltaBytes)
	d.anchored = true
	d.scannedAt = at
}

// snapshotDeltas reads the counters a scan starts from, so anchor can tell what
// moved underneath it.
func (d *diskIndex) snapshotDeltas() (fragments, bytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.fragments, d.bytes
}

// Occupancy reports what the disk holds, from the index.
//
// It costs nothing to ask — that is the point of keeping it — but the answer is
// only meaningful once Anchored, and it is an estimate even then. Do not gate a
// volume deletion on it; HasData answers that exactly.
func (s *Store) Occupancy(disk cluster.DiskID) (Occupancy, error) {
	idx, ok := s.index[disk]
	if !ok {
		return Occupancy{}, errors.Errorf("unknown disk %q", disk)
	}

	return idx.usage(), nil
}

// Scan walks one disk and anchors its counters to what is actually there.
//
// This is the expensive, authoritative half of the index: it stats every
// fragment, so it costs one syscall per file and belongs in the background, not
// on a request path. Call it once at startup (the counters mean nothing until
// it lands) and periodically after that to shed accumulated drift; the node
// runtime schedules both.
func (s *Store) Scan(ctx context.Context, disk cluster.DiskID) error {
	root, ok := s.roots[disk]
	if !ok {
		return errors.Errorf("unknown disk %q", disk)
	}

	idx := s.index[disk]

	// What the counters read before the walk, so concurrent writes survive it.
	startFragments, startBytes := idx.snapshotDeltas()

	var fragments, bytes int64

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				// Racing a delete and its directory prune is expected.
				return nil
			}

			return err
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if d.IsDir() || skipEntry(d.Name()) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}

			return errors.Wrap(err, "stat fragment")
		}

		fragments++
		bytes += info.Size()

		return nil
	})
	if err != nil {
		return errors.Wrapf(err, "scan disk %q", disk)
	}

	idx.anchor(fragments, bytes, startFragments, startBytes, time.Now().UTC())

	return nil
}

// ScanAll anchors every disk, stopping at the first failure. The node runtime
// runs it in the background at startup.
func (s *Store) ScanAll(ctx context.Context) error {
	for disk := range s.roots {
		if err := s.Scan(ctx, disk); err != nil {
			return err
		}
	}

	return nil
}

// Checkpoint writes every disk's counters so a later start can adopt them
// instead of rescanning. clean marks the counters as trustworthy, which only an
// orderly shutdown may claim.
func (s *Store) Checkpoint(clean bool) error {
	var firstErr error

	for disk, idx := range s.index {
		if err := s.checkpointDisk(disk, idx, clean); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// checkpointDisk writes one disk's checkpoint through the store's temp-file and
// rename protocol, so a crash mid-write leaves the previous checkpoint rather
// than a truncated one.
func (s *Store) checkpointDisk(disk cluster.DiskID, idx *diskIndex, clean bool) error {
	root := s.roots[disk]
	path := filepath.Join(root, checkpointName)

	u := idx.usage()
	if !u.Anchored {
		// Nothing worth persisting — and whatever is on disk is not worth
		// keeping either. It was already rejected once (wrong version, corrupt,
		// or left unclean); leaving it there only risks some later reader
		// adopting counters this process has already disowned.
		_ = os.Remove(path)

		return nil
	}

	data, err := json.Marshal(checkpoint{
		Version:   checkpointVersion,
		Fragments: u.Fragments,
		Bytes:     u.Bytes,
		ScannedAt: u.ScannedAt,
		Clean:     clean,
	})
	if err != nil {
		return errors.Wrap(err, "marshal checkpoint")
	}

	tmp, err := os.CreateTemp(root, tmpPattern)
	if err != nil {
		return errors.Wrapf(err, "create checkpoint for disk %q", disk)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())

		return errors.Wrapf(err, "write checkpoint for disk %q", disk)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())

		return errors.Wrapf(err, "close checkpoint for disk %q", disk)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())

		return errors.Wrapf(err, "commit checkpoint for disk %q", disk)
	}

	return nil
}

// loadCheckpoint adopts a clean checkpoint, if there is one.
//
// Anything else — missing, unreadable, corrupt, from another format version, or
// left unclean by a crash — leaves the disk unanchored, which schedules a scan.
// A wrong count is worse than a missing one here: the missing one says so.
func loadCheckpoint(root string) (checkpoint, bool) {
	data, err := os.ReadFile(filepath.Join(root, checkpointName)) //nolint:gosec // Path is the disk root plus a constant.
	if err != nil {
		return checkpoint{}, false
	}

	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return checkpoint{}, false
	}

	if cp.Version != checkpointVersion || !cp.Clean {
		return checkpoint{}, false
	}

	if cp.Fragments < 0 || cp.Bytes < 0 {
		return checkpoint{}, false
	}

	return cp, true
}
