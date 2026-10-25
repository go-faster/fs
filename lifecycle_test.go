package fs_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestLifecycleNoncurrentExpiry(t *testing.T) {
	t.Parallel()

	// Replaced at 10:30 on the 15th: three days on, rounded up to midnight,
	// is the 19th, as the S3 documentation's example works it.
	since := time.Date(2014, 1, 15, 10, 30, 0, 0, time.UTC)
	due := time.Date(2014, 1, 19, 0, 0, 0, 0, time.UTC)

	rules := []fs.LifecycleRule{
		{ID: "history", Status: fs.LifecycleEnabled, Prefix: "docs/", NoncurrentDays: 3, NewerNoncurrentVersions: 2},
	}

	t.Run("Due", func(t *testing.T) {
		t.Parallel()

		at, rule := fs.LifecycleNoncurrentExpiry(rules, "docs/a", since, 2)
		require.Equal(t, due, at)
		require.Equal(t, "history", rule)
	})
	t.Run("KeepsNewest", func(t *testing.T) {
		t.Parallel()

		// One newer noncurrent version: this is the second newest, kept.
		at, _ := fs.LifecycleNoncurrentExpiry(rules, "docs/a", since, 1)
		require.True(t, at.IsZero())
	})
	t.Run("OtherPrefix", func(t *testing.T) {
		t.Parallel()

		at, _ := fs.LifecycleNoncurrentExpiry(rules, "logs/a", since, 5)
		require.True(t, at.IsZero())
	})
	t.Run("Disabled", func(t *testing.T) {
		t.Parallel()

		off := []fs.LifecycleRule{{ID: "off", Status: fs.LifecycleDisabled, NoncurrentDays: 1}}
		at, _ := fs.LifecycleNoncurrentExpiry(off, "docs/a", since, 0)
		require.True(t, at.IsZero())
	})
	t.Run("EarliestWins", func(t *testing.T) {
		t.Parallel()

		both := append([]fs.LifecycleRule{
			{ID: "current", Status: fs.LifecycleEnabled, ExpirationDays: 1},
			{ID: "short", Status: fs.LifecycleEnabled, NoncurrentDays: 1},
		}, rules...)

		at, rule := fs.LifecycleNoncurrentExpiry(both, "docs/a", since, 2)
		require.Equal(t, time.Date(2014, 1, 17, 0, 0, 0, 0, time.UTC), at)
		require.Equal(t, "short", rule)
	})
}

func TestLifecycleMarkerExpiry(t *testing.T) {
	t.Parallel()

	written := time.Date(2014, 1, 15, 10, 30, 0, 0, time.UTC)

	t.Run("Marker", func(t *testing.T) {
		t.Parallel()

		rules := []fs.LifecycleRule{{ID: "m", Status: fs.LifecycleEnabled, ExpiredObjectDeleteMarker: true}}
		at, rule := fs.LifecycleMarkerExpiry(rules, "a", written)
		require.Equal(t, written, at)
		require.Equal(t, "m", rule)
	})
	t.Run("Days", func(t *testing.T) {
		t.Parallel()

		// An Expiration Days rule cleans markers up once they are its age.
		rules := []fs.LifecycleRule{{ID: "d", Status: fs.LifecycleEnabled, ExpirationDays: 3}}
		at, _ := fs.LifecycleMarkerExpiry(rules, "a", written)
		require.Equal(t, time.Date(2014, 1, 19, 0, 0, 0, 0, time.UTC), at)
	})
	t.Run("NotDate", func(t *testing.T) {
		t.Parallel()

		rules := []fs.LifecycleRule{
			{ID: "date", Status: fs.LifecycleEnabled, ExpirationDate: written},
			{ID: "noncurrent", Status: fs.LifecycleEnabled, NoncurrentDays: 1},
		}
		at, _ := fs.LifecycleMarkerExpiry(rules, "a", written)
		require.True(t, at.IsZero())
	})
}
