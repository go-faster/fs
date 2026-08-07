package lastrun_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/lastrun"
)

func TestDue(t *testing.T) {
	t.Parallel()

	const (
		interval = 12 * time.Hour
		floor    = time.Minute
	)

	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)

	t.Run("NeverRun", func(t *testing.T) {
		t.Parallel()

		// A fresh deployment is due, but still waits out the floor so a
		// crashloop cannot spin on it.
		require.Equal(t, floor, lastrun.Due(time.Time{}, now, interval, floor))
	})

	t.Run("Overdue", func(t *testing.T) {
		t.Parallel()

		// This is the restart case the whole package exists for: the pass is
		// long overdue, so it runs now rather than one interval from process
		// start.
		require.Equal(t, floor, lastrun.Due(now.Add(-24*time.Hour), now, interval, floor))
	})

	t.Run("PartWayThrough", func(t *testing.T) {
		t.Parallel()

		// Restarting four hours in leaves eight to go — not twelve, which is
		// what scheduling from process start would give.
		require.Equal(t, 8*time.Hour, lastrun.Due(now.Add(-4*time.Hour), now, interval, floor))
	})

	t.Run("JustRan", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, interval, lastrun.Due(now, now, interval, floor))
	})

	t.Run("BarelyDue", func(t *testing.T) {
		t.Parallel()

		// Within the floor of being due: the floor wins, never a shorter wait.
		require.Equal(t, floor, lastrun.Due(now.Add(-interval+time.Second), now, interval, floor))
	})

	t.Run("RecordFromTheFuture", func(t *testing.T) {
		t.Parallel()

		// A clock that jumped backwards, or a peer whose clock runs ahead, must
		// not park the pass for longer than the operator's interval.
		require.Equal(t, interval, lastrun.Due(now.Add(time.Hour), now, interval, floor))
	})
}

func TestFileRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := lastrun.NewFile(t.TempDir())

	// Never run reads as zero rather than an error: a fresh data directory is
	// not a failure.
	got, err := store.LastRun(ctx, "scrub")
	require.NoError(t, err)
	require.True(t, got.IsZero())

	at := time.Date(2026, time.August, 7, 9, 30, 0, 0, time.UTC)
	require.NoError(t, store.SetLastRun(ctx, "scrub", at))

	got, err = store.LastRun(ctx, "scrub")
	require.NoError(t, err)
	require.True(t, at.Equal(got), "%s != %s", at, got)

	// Tasks are independent: recording one must not answer for the other.
	got, err = store.LastRun(ctx, "lifecycle")
	require.NoError(t, err)
	require.True(t, got.IsZero())

	later := at.Add(time.Hour)
	require.NoError(t, store.SetLastRun(ctx, "scrub", later))

	got, err = store.LastRun(ctx, "scrub")
	require.NoError(t, err)
	require.True(t, later.Equal(got), "%s != %s", later, got)
}

// TestFileSurvivesACorruptRecord: a damaged state file must schedule the pass,
// not abandon it. Reporting "never" is the safe direction — one extra pass
// instead of a scrub or sweep that silently stops.
func TestFileSurvivesACorruptRecord(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := t.TempDir()
	store := lastrun.NewFile(dir)

	require.NoError(t, store.SetLastRun(ctx, "scrub", time.Now()))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir, "scrub.json"), []byte("{not json"), 0o600))

	got, err := store.LastRun(ctx, "scrub")
	require.NoError(t, err)
	require.True(t, got.IsZero())
}
