package lifecycle_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/lifecycle"
	"github.com/go-faster/fs/storagemem"
)

const testBucket = "bucket"

// put writes an object and returns nothing; the sweep reads it back through the
// listing, which is where LastModified comes from.
func put(t *testing.T, storage fs.Storage, key, body string) {
	t.Helper()

	_, err := storage.PutObject(t.Context(), &fs.PutObjectRequest{
		Bucket: testBucket,
		Key:    key,
		Reader: strings.NewReader(body),
		Size:   int64(len(body)),
	})
	require.NoError(t, err)
}

func keys(t *testing.T, storage fs.Storage) []string {
	t.Helper()

	page, err := storage.ListObjects(t.Context(), &fs.ListObjectsRequest{Bucket: testBucket})
	require.NoError(t, err)

	out := make([]string, 0, len(page.Objects))
	for _, o := range page.Objects {
		out = append(out, o.Key)
	}

	return out
}

// TestSweepExpires is the guard on the half of lifecycle that actually deletes:
// a matching object past its Days is removed, and everything the rules do not
// name is left alone.
func TestSweepExpires(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := storagemem.New()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	put(t, storage, "logs/old.txt", "old")
	put(t, storage, "logs/nested/also-old.txt", "old")
	put(t, storage, "keep/mine.txt", "keep")
	put(t, storage, "parked/parked.txt", "parked")

	require.NoError(t, storage.SetBucketLifecycle(ctx, testBucket, []fs.LifecycleRule{
		{ID: "logs", Status: fs.LifecycleEnabled, Prefix: "logs/", ExpirationDays: 7},
		{ID: "parked", Status: fs.LifecycleDisabled, Prefix: "parked/", ExpirationDays: 1},
	}))

	sweeper := &lifecycle.Sweeper{Storage: storage}

	// Before the rule comes due nothing moves, which is the half of the
	// behavior a sweep that deleted everything would also pass.
	report, err := sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Zero(t, report.Expired)
	require.Len(t, keys(t, storage), 4)

	// Eight days on, both objects under logs/ are past a 7-day rule; the
	// disabled rule's prefix and the unmatched one are not.
	sweeper.Now = func() time.Time { return time.Now().Add(8 * fs.LifecycleDay) }

	report, err = sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.Buckets)
	require.Equal(t, 2, report.Expired)
	require.Zero(t, report.Raced)

	require.ElementsMatch(t, []string{"keep/mine.txt", "parked/parked.txt"}, keys(t, storage))
}

// TestSweepExpiresByDate covers the Date form, which expires everything under
// the prefix at once rather than by each object's own age.
func TestSweepExpiresByDate(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := storagemem.New()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	put(t, storage, "archive/one.txt", "one")
	put(t, storage, "live/two.txt", "two")

	deadline := time.Now().Add(24 * time.Hour)

	require.NoError(t, storage.SetBucketLifecycle(ctx, testBucket, []fs.LifecycleRule{
		{ID: "archive", Status: fs.LifecycleEnabled, Prefix: "archive/", ExpirationDate: deadline},
	}))

	sweeper := &lifecycle.Sweeper{Storage: storage}

	report, err := sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Zero(t, report.Expired)

	sweeper.Now = func() time.Time { return deadline.Add(time.Minute) }

	report, err = sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.Expired)
	require.Equal(t, []string{"live/two.txt"}, keys(t, storage))
}

// TestSweepAbortsAbandonedUploads covers the other action: a multipart upload
// left unfinished past DaysAfterInitiation is aborted, which is what stops
// abandoned parts from being an invisible disk leak.
func TestSweepAbortsAbandonedUploads(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := storagemem.New()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))

	up, err := storage.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: testBucket,
		Key:    "uploads/big.bin",
	})
	require.NoError(t, err)

	require.NoError(t, storage.SetBucketLifecycle(ctx, testBucket, []fs.LifecycleRule{
		{ID: "abandoned", Status: fs.LifecycleEnabled, AbortIncompleteMultipartUploadDays: 3},
	}))

	sweeper := &lifecycle.Sweeper{Storage: storage}

	report, err := sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Zero(t, report.Aborted)

	sweeper.Now = func() time.Time { return time.Now().Add(4 * fs.LifecycleDay) }

	report, err = sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.Aborted)

	uploads, err := storage.ListMultipartUploads(ctx, testBucket)
	require.NoError(t, err)
	require.Empty(t, uploads)

	_, err = storage.ListParts(ctx, testBucket, up.Key, up.UploadID)
	require.ErrorIs(t, err, fs.ErrUploadNotFound)
}

// TestSweepSkipsRewrittenObject is the data-loss guard.
//
// Expiry is decided from a listing that is stale by the time the delete runs.
// An unfenced delete would destroy the fresh object a client wrote in between —
// the cleanup meant to be safe eating a brand-new write — so the delete is
// conditional on the ETag the listing saw, and a rewritten object survives.
func TestSweepSkipsRewrittenObject(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := storagemem.New()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	put(t, storage, "logs/racy.txt", "stale")

	require.NoError(t, storage.SetBucketLifecycle(ctx, testBucket, []fs.LifecycleRule{
		{ID: "logs", Status: fs.LifecycleEnabled, Prefix: "logs/", ExpirationDays: 1},
	}))

	// rewriteOnList stands in for the client that overwrites the key between
	// the sweep's listing and its delete.
	racy := &rewriteOnList{Storage: storage, t: t}

	sweeper := &lifecycle.Sweeper{
		Storage: racy,
		Now:     func() time.Time { return time.Now().Add(3 * fs.LifecycleDay) },
	}

	report, err := sweeper.Sweep(ctx)
	require.NoError(t, err)
	require.Zero(t, report.Expired, "a rewritten object must not be expired on the stale listing")
	require.Equal(t, 1, report.Raced)

	got, err := storage.GetObject(ctx, testBucket, "logs/racy.txt")
	require.NoError(t, err)
	require.NoError(t, got.Reader.Close())
	require.Equal(t, []string{"logs/racy.txt"}, keys(t, storage))
}

// expiringBucket sets up one bucket whose single object is already past a
// one-day rule.
func expiringBucket(t *testing.T) *storagemem.Storage {
	t.Helper()

	storage := storagemem.New()
	require.NoError(t, storage.CreateBucket(t.Context(), testBucket))
	put(t, storage, "logs/old.txt", "old")
	require.NoError(t, storage.SetBucketLifecycle(t.Context(), testBucket, []fs.LifecycleRule{
		{ID: "logs", Status: fs.LifecycleEnabled, Prefix: "logs/", ExpirationDays: 1},
	}))

	return storage
}

// TestRunSweepsWhenOverdue is the guard on restart behavior.
//
// Run must not wait a whole interval for a sweep that is already due: a node
// restarted more often than the interval would then never sweep at all, and on
// a deployment that redeploys hourly with a 12h interval, lifecycle rules would
// silently stop applying.
func TestRunSweepsWhenOverdue(t *testing.T) {
	t.Parallel()

	storage := expiringBucket(t)

	sweeper := &lifecycle.Sweeper{
		Storage: storage,
		Now:     func() time.Time { return time.Now().Add(3 * fs.LifecycleDay) },
		State:   &memState{},
		Floor:   time.Millisecond,
	}

	runCtx, stop := context.WithCancel(t.Context())
	defer stop()

	go sweeper.Run(runCtx, time.Hour)

	// The interval is an hour; the object has to be gone long before that.
	require.Eventually(t, func() bool {
		return len(keys(t, storage)) == 0
	}, 5*time.Second, 5*time.Millisecond, "an overdue sweep must not wait a full interval")
}

// TestRunHonorsARecentSweep is the other half, and the reason the record
// exists: a pass that just ran must not be repeated because the process
// restarted. Without it, a node that restarts every few minutes re-lists every
// bucket in the deployment each time.
func TestRunHonorsARecentSweep(t *testing.T) {
	t.Parallel()

	storage := expiringBucket(t)

	// The object is three days old by the sweeper's clock, and would expire on
	// any pass that ran — so if it survives, it is the schedule that spared it.
	now := time.Now().Add(3 * fs.LifecycleDay)

	// Recorded as swept a moment ago on that same clock, so the next pass is a
	// full interval away.
	state := &memState{}
	require.NoError(t, state.SetLastRun(t.Context(), lifecycle.DefaultTask, now))

	sweeper := &lifecycle.Sweeper{
		Storage: storage,
		Now:     func() time.Time { return now },
		State:   state,
		Floor:   time.Millisecond,
	}

	runCtx, stop := context.WithCancel(t.Context())
	defer stop()

	go sweeper.Run(runCtx, time.Hour)

	require.Never(t, func() bool {
		return len(keys(t, storage)) == 0
	}, 250*time.Millisecond, 25*time.Millisecond, "a sweep recorded moments ago must not run again on start")
}

// TestRunRecordsAQuietSweep: a pass that found nothing to delete is still a
// pass. If it were not recorded, every deployment with no expiring objects
// would re-list its buckets on each restart — the cost the record exists to
// avoid, paid by exactly the deployments with nothing to gain from it.
func TestRunRecordsAQuietSweep(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	storage := storagemem.New()

	require.NoError(t, storage.CreateBucket(ctx, testBucket))
	put(t, storage, "keep/mine.txt", "keep")
	require.NoError(t, storage.SetBucketLifecycle(ctx, testBucket, []fs.LifecycleRule{
		{ID: "logs", Status: fs.LifecycleEnabled, Prefix: "logs/", ExpirationDays: 1},
	}))

	state := &memState{}
	sweeper := &lifecycle.Sweeper{Storage: storage, State: state, Floor: time.Millisecond}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	go sweeper.Run(runCtx, time.Hour)

	require.Eventually(t, func() bool {
		at, err := state.LastRun(ctx, lifecycle.DefaultTask)
		require.NoError(t, err)

		return !at.IsZero()
	}, 5*time.Second, 5*time.Millisecond, "a sweep that deleted nothing must still be recorded")
}

// memState is an in-memory lastrun.Store.
type memState struct {
	mu    sync.Mutex
	times map[string]time.Time
}

func (m *memState) LastRun(_ context.Context, task string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.times[task], nil
}

func (m *memState) SetLastRun(_ context.Context, task string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.times == nil {
		m.times = make(map[string]time.Time)
	}

	m.times[task] = at

	return nil
}

// rewriteOnList overwrites the listed key once, right after the listing the
// sweep decided from.
//
// It embeds the concrete backend rather than fs.Storage so the optional
// capabilities — storing lifecycle rules, deleting conditionally — stay
// reachable through the wrapper; an embedded fs.Storage would hide exactly the
// interface under test.
type rewriteOnList struct {
	*storagemem.Storage

	t    *testing.T
	done bool
}

func (r *rewriteOnList) ListObjects(ctx context.Context, req *fs.ListObjectsRequest) (*fs.ListObjectsResponse, error) {
	page, err := r.Storage.ListObjects(ctx, req)
	if err != nil || r.done {
		return page, err
	}

	r.done = true

	for _, o := range page.Objects {
		put(r.t, r.Storage, o.Key, "fresh")
	}

	return page, nil
}
