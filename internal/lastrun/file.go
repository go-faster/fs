package lastrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/go-faster/errors"
)

// stateDirPermissions matches the rest of the data directory.
const stateDirPermissions = 0o750

// File records pass completions under a directory, one small JSON document per
// task. It is the single-node store: there is no control plane to ask, and the
// data directory is the one thing that outlives the process.
//
// One file per task rather than one shared document, so two passes recording
// completion at the same moment cannot lose each other's write — no lock, and
// no chance of a scrub's timestamp erasing the sweeper's.
type File struct {
	// Dir holds the documents; created on first write.
	Dir string
}

// NewFile returns a store under root/.lastrun. The leading dot keeps it out of
// the way of bucket directories, which is also why it cannot collide with one:
// a bucket name may not start with a dot.
func NewFile(root string) *File {
	return &File{Dir: filepath.Join(root, ".lastrun")}
}

// record is the on-disk document. It is a struct rather than a bare timestamp
// so a later field (what the pass found, how long it took) does not have to
// break the format.
type record struct {
	CompletedAt time.Time `json:"completed_at"`
}

func (f *File) path(task string) string {
	return filepath.Join(f.Dir, task+".json")
}

// LastRun implements Store. A missing or unreadable record reads as never,
// which schedules the pass rather than skipping it: the failure mode of a
// corrupt state file should be an extra pass, not a silently abandoned one.
func (f *File) LastRun(_ context.Context, task string) (time.Time, error) {
	data, err := os.ReadFile(f.path(task)) //nolint:gosec // Path built from a caller-supplied task name.
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}

		return time.Time{}, errors.Wrapf(err, "read %s last-run record", task)
	}

	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return time.Time{}, nil
	}

	return rec.CompletedAt, nil
}

// SetLastRun implements Store, replacing the record atomically so a crash
// mid-write leaves the previous timestamp rather than a truncated file.
func (f *File) SetLastRun(_ context.Context, task string, t time.Time) error {
	if err := os.MkdirAll(f.Dir, stateDirPermissions); err != nil {
		return errors.Wrap(err, "create last-run directory")
	}

	data, err := json.Marshal(record{CompletedAt: t.UTC()})
	if err != nil {
		return errors.Wrap(err, "marshal last-run record")
	}

	path := f.path(task)

	tmp, err := os.CreateTemp(f.Dir, task+".*.tmp")
	if err != nil {
		return errors.Wrap(err, "create last-run temp file")
	}

	// Best-effort cleanup: after a successful rename there is nothing at the
	// temp name and the remove fails harmlessly.
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return errors.Wrap(err, "write last-run record")
	}

	if err := tmp.Close(); err != nil {
		return errors.Wrap(err, "close last-run record")
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Wrap(err, "replace last-run record")
	}

	return nil
}
