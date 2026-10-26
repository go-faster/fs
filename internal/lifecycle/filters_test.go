package lifecycle_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagemem"
)

var tmpTag = fs.Tag{Key: "tmp", Value: "true"}

func putTagged(t *testing.T, storage fs.Storage, key, body string, tags ...fs.Tag) {
	t.Helper()

	_, err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket: testBucket,
		Key:    key,
		Reader: strings.NewReader(body),
		Size:   int64(len(body)),
		Tags:   tags,
	})
	require.NoError(t, err)
}

func memWithRules(t *testing.T, rules ...fs.LifecycleRule) *storagemem.Storage {
	t.Helper()

	s := storagemem.New()
	require.NoError(t, s.CreateBucket(t.Context(), testBucket))
	require.NoError(t, s.SetBucketLifecycle(t.Context(), testBucket, rules))

	return s
}

// TestSweepTagFilter expires only the objects carrying the rule's tag, and
// judges tags as they are at the pass: one tagged after its write is
// expired too, counted from the write.
func TestSweepTagFilter(t *testing.T) {
	t.Parallel()

	s := memWithRules(t, fs.LifecycleRule{
		ID: "tmp", Status: fs.LifecycleEnabled, Prefix: "logs/", Tags: []fs.Tag{tmpTag}, ExpirationDays: 1,
	})

	putTagged(t, s, "logs/tagged", "x", tmpTag)
	putTagged(t, s, "logs/plain", "x")
	putTagged(t, s, "logs/other-value", "x", fs.Tag{Key: "tmp", Value: "false"})
	putTagged(t, s, "other/tagged", "x", tmpTag)

	report := sweepAt(t, s, 3*fs.LifecycleDay)
	require.Equal(t, 1, report.Expired)
	require.Equal(t, []string{"logs/other-value", "logs/plain", "other/tagged"}, keys(t, s))

	require.NoError(t, s.PutObjectTagging(t.Context(), testBucket, "logs/plain", []fs.Tag{tmpTag}))

	report = sweepAt(t, s, 3*fs.LifecycleDay)
	require.Equal(t, 1, report.Expired)
	require.Equal(t, []string{"logs/other-value", "other/tagged"}, keys(t, s))
}

// TestSweepSizeFilter expires objects strictly between the bounds.
func TestSweepSizeFilter(t *testing.T) {
	t.Parallel()

	gt, lt := int64(3), int64(6)
	s := memWithRules(t, fs.LifecycleRule{
		ID: "mid", Status: fs.LifecycleEnabled, SizeGreaterThan: &gt, SizeLessThan: &lt, ExpirationDays: 1,
	})

	for _, body := range []string{"", "333", "4444", "55555", "666666"} {
		put(t, s, "size"+string(rune('0'+len(body))), body)
	}

	report := sweepAt(t, s, 3*fs.LifecycleDay)
	require.Equal(t, 2, report.Expired)
	require.Equal(t, []string{"size0", "size3", "size6"}, keys(t, s))
}

// TestSweepNoncurrentByOwnTags judges each noncurrent version by its own
// tags, not the current version's.
func TestSweepNoncurrentByOwnTags(t *testing.T) {
	t.Parallel()

	e := versionedEngine(t, fs.LifecycleRule{
		ID: "tmp", Status: fs.LifecycleEnabled, Tags: []fs.Tag{tmpTag}, NoncurrentDays: 1,
	})

	putTagged(t, e, "k", "v1", tmpTag)
	putTagged(t, e, "k", "v2")
	putTagged(t, e, "k", "v3", tmpTag)

	report := sweepAt(t, e, 3*fs.LifecycleDay)
	require.Equal(t, 1, report.Noncurrent)
	require.Equal(t, []string{"v3", "v2"}, bodies(t, e, "k"))
}

// TestSweepUnmatchedKeepsMarker keeps a delete marker while a version no rule
// matches is still under it.
func TestSweepUnmatchedKeepsMarker(t *testing.T) {
	t.Parallel()

	e := versionedEngine(t,
		fs.LifecycleRule{ID: "tmp", Status: fs.LifecycleEnabled, Tags: []fs.Tag{tmpTag}, NoncurrentDays: 1},
		fs.LifecycleRule{ID: "markers", Status: fs.LifecycleEnabled, ExpiredObjectDeleteMarker: true},
	)

	putTagged(t, e, "k", "kept")
	putTagged(t, e, "k", "scratch", tmpTag)

	_, err := e.DeleteObjectVersion(t.Context(), testBucket, "k", "")
	require.NoError(t, err)

	report := sweepAt(t, e, 3*fs.LifecycleDay)
	require.Equal(t, 1, report.Noncurrent)
	require.Zero(t, report.Markers)
	require.Equal(t, []string{"marker", "kept"}, bodies(t, e, "k"))
}

// tagsRequested records whether each listing asked for tags.
type tagsRequested struct {
	*storagemem.Storage

	asked []bool
}

func (r *tagsRequested) ListObjects(ctx context.Context, req *fs.ListObjectsRequest) (*fs.ListObjectsResponse, error) {
	r.asked = append(r.asked, req.Tags)

	return r.Storage.ListObjects(ctx, req)
}

// TestSweepListsTagsOnlyForTagRules: decoding tags is paid only by a bucket
// with an enabled tag rule.
func TestSweepListsTagsOnlyForTagRules(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rule fs.LifecycleRule
		want bool
	}{
		{"Prefix", fs.LifecycleRule{ID: "p", Status: fs.LifecycleEnabled, Prefix: "a/", ExpirationDays: 1}, false},
		{"Tag", fs.LifecycleRule{ID: "t", Status: fs.LifecycleEnabled, Tags: []fs.Tag{tmpTag}, ExpirationDays: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A disabled tag rule beside it must not make the listing ask.
			disabled := fs.LifecycleRule{ID: "off", Status: fs.LifecycleDisabled, Tags: []fs.Tag{tmpTag}, ExpirationDays: 1}
			s := &tagsRequested{Storage: memWithRules(t, tc.rule, disabled)}
			put(t, s, "a/x", "x")

			sweepAt(t, s, 0)
			require.Equal(t, []bool{tc.want}, s.asked)
		})
	}
}
