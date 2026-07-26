package diskstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// scrubStateName is the per-disk scrub cursor file, at the disk root.
//
// Dot-named for the same reason the occupancy checkpoint is: the fragment
// walkers treat every dot-prefixed entry as store bookkeeping, and a plain file
// here would read as a fragment — making a drained disk report occupied forever
// and hanging the decommission that waits on it.
const scrubStateName = ".scrub.json"

// scrubStateVersion stamps the format. A file from another version is ignored
// rather than guessed at: the cost is one restarted pass.
const scrubStateVersion = 1

// scrubStateFile is the on-disk form of cluster.ScrubState.
type scrubStateFile struct {
	Version       int       `json:"version"`
	Cursor        string    `json:"cursor,omitempty"`
	PassStarted   time.Time `json:"pass_started,omitzero"`
	LastCompleted time.Time `json:"last_completed,omitzero"`
}

// ScrubStateStore returns the disk-backed store the repairer records scrub
// progress in. It satisfies clusterstore.ScrubStateStore structurally, so the
// dependency runs one way only: the coordinator declares what it needs, and
// nothing down here learns about coordinators.
func (s *Store) ScrubStateStore() ScrubStateStore { return ScrubStateStore{s} }

// ScrubStateStore persists scrub cursors on the disks they describe.
type ScrubStateStore struct{ s *Store }

// LoadScrubState implements clusterstore.ScrubStateStore. Anything unreadable —
// missing, corrupt, or from another format version — reads as "no pass in
// flight", which costs a restarted sweep and never correctness.
func (t ScrubStateStore) LoadScrubState(disk cluster.DiskID) cluster.ScrubState {
	root, ok := t.s.roots[disk]
	if !ok {
		return cluster.ScrubState{}
	}

	data, err := os.ReadFile(filepath.Join(root, scrubStateName)) //nolint:gosec // Path is the disk root plus a constant.
	if err != nil {
		return cluster.ScrubState{}
	}

	var f scrubStateFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != scrubStateVersion {
		return cluster.ScrubState{}
	}

	return cluster.ScrubState{
		Cursor:        f.Cursor,
		PassStarted:   f.PassStarted,
		LastCompleted: f.LastCompleted,
	}
}

// SaveScrubState implements clusterstore.ScrubStateStore, writing through the
// store's temp-file and rename protocol so a crash mid-write leaves the
// previous cursor rather than a truncated one.
func (t ScrubStateStore) SaveScrubState(disk cluster.DiskID, state cluster.ScrubState) error {
	root, ok := t.s.roots[disk]
	if !ok {
		return errors.Errorf("unknown disk %q", disk)
	}

	data, err := json.Marshal(scrubStateFile{
		Version:       scrubStateVersion,
		Cursor:        state.Cursor,
		PassStarted:   state.PassStarted,
		LastCompleted: state.LastCompleted,
	})
	if err != nil {
		return errors.Wrap(err, "marshal scrub state")
	}

	tmp, err := os.CreateTemp(root, tmpPattern)
	if err != nil {
		return errors.Wrapf(err, "create scrub state for disk %q", disk)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())

		return errors.Wrapf(err, "write scrub state for disk %q", disk)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())

		return errors.Wrapf(err, "close scrub state for disk %q", disk)
	}

	if err := os.Rename(tmp.Name(), filepath.Join(root, scrubStateName)); err != nil {
		_ = os.Remove(tmp.Name())

		return errors.Wrapf(err, "commit scrub state for disk %q", disk)
	}

	return nil
}
