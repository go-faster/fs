package fs_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

var docsA = fs.LifecycleObject{Key: "docs/a"}

func size(n int64) *int64 { return &n }

func TestLifecycleRuleMatches(t *testing.T) {
	t.Parallel()

	tmp := fs.Tag{Key: "tmp", Value: "true"}
	team := fs.Tag{Key: "team", Value: "storage"}

	for _, tt := range []struct {
		name string
		rule fs.LifecycleRule
		obj  fs.LifecycleObject
		want bool
	}{
		{"EmptyFilter", fs.LifecycleRule{}, fs.LifecycleObject{Key: "a", Size: 5}, true},
		{"Prefix", fs.LifecycleRule{Prefix: "logs/"}, fs.LifecycleObject{Key: "logs/a"}, true},
		{"OtherPrefix", fs.LifecycleRule{Prefix: "logs/"}, fs.LifecycleObject{Key: "docs/a"}, false},
		{"Tag", fs.LifecycleRule{Tags: []fs.Tag{tmp}}, fs.LifecycleObject{Key: "a", Tags: []fs.Tag{team, tmp}}, true},
		{"TagMissing", fs.LifecycleRule{Tags: []fs.Tag{tmp}}, fs.LifecycleObject{Key: "a", Tags: []fs.Tag{team}}, false},
		{"TagOtherValue", fs.LifecycleRule{Tags: []fs.Tag{tmp}}, fs.LifecycleObject{Key: "a", Tags: []fs.Tag{{Key: "tmp", Value: "false"}}}, false},
		{"EveryTag", fs.LifecycleRule{Tags: []fs.Tag{tmp, team}}, fs.LifecycleObject{Key: "a", Tags: []fs.Tag{tmp}}, false},
		{"EmptyValue", fs.LifecycleRule{Tags: []fs.Tag{{Key: "tmp"}}}, fs.LifecycleObject{Key: "a", Tags: []fs.Tag{{Key: "tmp"}}}, true},
		// Both bounds are strict.
		{"GreaterAtBound", fs.LifecycleRule{SizeGreaterThan: size(100)}, fs.LifecycleObject{Key: "a", Size: 100}, false},
		{"GreaterAbove", fs.LifecycleRule{SizeGreaterThan: size(100)}, fs.LifecycleObject{Key: "a", Size: 101}, true},
		{"GreaterZeroEmpty", fs.LifecycleRule{SizeGreaterThan: size(0)}, fs.LifecycleObject{Key: "a"}, false},
		{"LessAtBound", fs.LifecycleRule{SizeLessThan: size(100)}, fs.LifecycleObject{Key: "a", Size: 100}, false},
		{"LessBelow", fs.LifecycleRule{SizeLessThan: size(100)}, fs.LifecycleObject{Key: "a", Size: 99}, true},
		{
			"And",
			fs.LifecycleRule{Prefix: "logs/", Tags: []fs.Tag{tmp}, SizeGreaterThan: size(10), SizeLessThan: size(100)},
			fs.LifecycleObject{Key: "logs/a", Size: 50, Tags: []fs.Tag{tmp}},
			true,
		},
		{
			"AndOneFails",
			fs.LifecycleRule{Prefix: "logs/", Tags: []fs.Tag{tmp}, SizeGreaterThan: size(10), SizeLessThan: size(100)},
			fs.LifecycleObject{Key: "logs/a", Size: 100, Tags: []fs.Tag{tmp}},
			false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, tt.rule.Matches(tt.obj))
		})
	}
}

func TestLifecycleExpiryFilters(t *testing.T) {
	t.Parallel()

	written := time.Date(2014, 1, 15, 10, 30, 0, 0, time.UTC)
	rules := []fs.LifecycleRule{
		{ID: "logs", Status: fs.LifecycleEnabled, Prefix: "logs/", ExpirationDays: 30},
		{ID: "tmp", Status: fs.LifecycleEnabled, Tags: []fs.Tag{{Key: "tmp", Value: "true"}}, ExpirationDays: 1},
	}

	// Both rules match a tagged log: the earlier expiry wins.
	at, rule := fs.LifecycleExpiry(rules, fs.LifecycleObject{Key: "logs/a", Tags: []fs.Tag{{Key: "tmp", Value: "true"}}}, written)
	require.Equal(t, time.Date(2014, 1, 17, 0, 0, 0, 0, time.UTC), at)
	require.Equal(t, "tmp", rule)

	_, rule = fs.LifecycleExpiry(rules, fs.LifecycleObject{Key: "logs/a"}, written)
	require.Equal(t, "logs", rule)
}

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

		at, rule := fs.LifecycleNoncurrentExpiry(rules, docsA, since, 2)
		require.Equal(t, due, at)
		require.Equal(t, "history", rule)
	})
	t.Run("KeepsNewest", func(t *testing.T) {
		t.Parallel()

		// One newer noncurrent version: this is the second newest, kept.
		at, _ := fs.LifecycleNoncurrentExpiry(rules, docsA, since, 1)
		require.True(t, at.IsZero())
	})
	t.Run("OtherPrefix", func(t *testing.T) {
		t.Parallel()

		at, _ := fs.LifecycleNoncurrentExpiry(rules, fs.LifecycleObject{Key: "logs/a"}, since, 5)
		require.True(t, at.IsZero())
	})
	t.Run("Disabled", func(t *testing.T) {
		t.Parallel()

		off := []fs.LifecycleRule{{ID: "off", Status: fs.LifecycleDisabled, NoncurrentDays: 1}}
		at, _ := fs.LifecycleNoncurrentExpiry(off, docsA, since, 0)
		require.True(t, at.IsZero())
	})
	t.Run("EarliestWins", func(t *testing.T) {
		t.Parallel()

		both := append([]fs.LifecycleRule{
			{ID: "current", Status: fs.LifecycleEnabled, ExpirationDays: 1},
			{ID: "short", Status: fs.LifecycleEnabled, NoncurrentDays: 1},
		}, rules...)

		at, rule := fs.LifecycleNoncurrentExpiry(both, docsA, since, 2)
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
	t.Run("NotByTag", func(t *testing.T) {
		t.Parallel()

		// A marker has no tags, so no tag rule removes it.
		rules := []fs.LifecycleRule{{ID: "t", Status: fs.LifecycleEnabled, Tags: []fs.Tag{{Key: "k", Value: "v"}}, ExpirationDays: 1}}
		at, _ := fs.LifecycleMarkerExpiry(rules, "a", written)
		require.True(t, at.IsZero())
	})
	t.Run("ZeroBytes", func(t *testing.T) {
		t.Parallel()

		// A marker is judged as zero bytes: under any upper bound, never
		// over a lower one.
		under := []fs.LifecycleRule{{ID: "u", Status: fs.LifecycleEnabled, SizeLessThan: size(1), ExpiredObjectDeleteMarker: true}}
		at, _ := fs.LifecycleMarkerExpiry(under, "a", written)
		require.Equal(t, written, at)

		over := []fs.LifecycleRule{{ID: "o", Status: fs.LifecycleEnabled, SizeGreaterThan: size(0), ExpiredObjectDeleteMarker: true}}
		at, _ = fs.LifecycleMarkerExpiry(over, "a", written)
		require.True(t, at.IsZero())
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
