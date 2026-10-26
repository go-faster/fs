// Package lifecycle enforces bucket lifecycle rules: it deletes objects that
// have expired, removes noncurrent versions and expired object delete markers
// of versioned buckets, and aborts multipart uploads that were abandoned.
//
// Enforcement is sweep-only, which is what S3 does: an expired object is still
// served until a pass actually removes it, and removal is eventual within one
// interval rather than instant. The alternative — hiding an expired object on
// read — would make listings and GETs disagree with what is on disk, and would
// still leave the bytes there.
//
// The sweep is written against fs.Storage alone, so it enforces the same rules
// on every backend: deletions go through the ordinary delete path, and cluster
// fan-out, accounting and metrics see them as the ordinary deletes they are.
package lifecycle

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"go.uber.org/zap"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/lastrun"
)

// sweepPage is how many keys a listing pass asks for at a time. The sweep walks
// whole buckets, so it pages rather than asking for everything: a bucket is
// unbounded and a pass must not need it in memory.
const sweepPage = 1000

// DefaultTask names the sweep's last-run record. It is cluster-wide rather than
// per node: one elected sweeper enforces the rules for everyone, so a pass by
// whichever node holds the election is a pass for the whole cluster.
const DefaultTask = "lifecycle"

// defaultFloor is the shortest Run waits before a sweep that is already due.
//
// Completion is recorded only when a pass finishes, so a node that dies
// part-way through comes back with the sweep still overdue. The floor is what
// keeps that from becoming a re-listing of every bucket on every restart of a
// crashlooping node.
const defaultFloor = time.Minute

// Sweeper deletes what a bucket's lifecycle rules say should be gone.
type Sweeper struct {
	// Storage is swept. A backend that cannot store lifecycle rules has none
	// to enforce, and the sweep is a no-op against it.
	Storage fs.Storage
	// Log receives one line per pass and per failure; nil discards them.
	Log *zap.Logger
	// Now supplies the current time. Nil means time.Now — tests move it
	// forward instead of waiting for a day to pass.
	Now func() time.Time
	// State remembers when a pass last completed, so restarting neither
	// postpones the sweep by a whole interval nor repeats it every time.
	//
	// Nil gives a sweeper with no memory: every start sweeps once the floor
	// elapses, which is what a caller that does not persist anything wants.
	State lastrun.Store
	// Task names the record in State; empty means DefaultTask.
	Task string
	// Floor is the shortest wait before an already-due sweep; zero means
	// defaultFloor.
	Floor time.Duration
	// Owns reports whether this sweeper enforces a bucket's rules; nil means
	// every bucket. In a cluster each bucket is given to one node, so the
	// nodes do not list the same bucket and race each other's deletes.
	Owns func(bucket string) bool

	mu    sync.Mutex
	stats Stats
}

// Report is what one pass did.
type Report struct {
	// Buckets is how many buckets carried enabled rules.
	Buckets int
	// Expired and Aborted are objects deleted and uploads aborted.
	Expired, Aborted int
	// Noncurrent is noncurrent versions removed, and Markers expired object
	// delete markers removed.
	Noncurrent, Markers int
	// Raced is objects that expired by the listing but changed before the
	// delete, and so were left alone. They expire on a later pass if the write
	// that overtook them did not reset their age past the rule.
	Raced int
	// Failed is buckets the pass could not finish: their rules unreadable, or
	// a listing or removal failed. The next pass starts them over.
	Failed int
}

func (r *Report) add(o Report) {
	r.Buckets += o.Buckets
	r.Expired += o.Expired
	r.Aborted += o.Aborted
	r.Noncurrent += o.Noncurrent
	r.Markers += o.Markers
	r.Raced += o.Raced
	r.Failed += o.Failed
}

// Stats is what the sweeper has done, for an operator to watch.
type Stats struct {
	// LastPass is when a pass last completed, as State recorded it; zero
	// when none has.
	LastPass time.Time
	// Total is every pass since start, added up.
	Total Report
	// Failed is buckets the last pass could not finish.
	Failed int
}

// Stats returns what the sweeper has done.
func (s *Sweeper) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.stats
}

func (s *Sweeper) record(report Report) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stats.Total.add(report)
	s.stats.Failed = report.Failed
}

func (s *Sweeper) passed(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stats.LastPass = at
}

func (s *Sweeper) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}

	return s.Now()
}

func (s *Sweeper) log() *zap.Logger {
	if s.Log == nil {
		return zap.NewNop()
	}

	return s.Log
}

func (s *Sweeper) task() string {
	if s.Task == "" {
		return DefaultTask
	}

	return s.Task
}

// lastRun reads the recorded completion; a sweeper with no State has no memory,
// which reads as never.
func (s *Sweeper) lastRun(ctx context.Context) (time.Time, error) {
	if s.State == nil {
		return time.Time{}, nil
	}

	return s.State.LastRun(ctx, s.task())
}

func (s *Sweeper) setLastRun(ctx context.Context, at time.Time) error {
	if s.State == nil {
		return nil
	}

	return s.State.SetLastRun(ctx, s.task(), at)
}

// Run sweeps until ctx is canceled: the first pass one interval after the last
// one State recorded, and every interval after that. A non-positive interval
// disables enforcement, which is why the S3 layer must not accept rules when
// the sweep is off: stored rules nothing enforces are a lie to the client.
//
// Scheduling from the recorded time rather than from process start is what
// makes the sweep survive restarts. A plain ticker would put the first pass a
// whole interval away, so a node redeployed hourly under the 12h default would
// never sweep at all — lifecycle rules silently inert on the deployments that
// restart most, which is the exact failure this feature exists to prevent.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	floor := s.Floor
	if floor <= 0 {
		floor = defaultFloor
	}

	// An unreadable record is not a reason to stop sweeping: the pass falls
	// back to "due now", the same answer a fresh deployment gets.
	last, err := s.lastRun(ctx)
	if err != nil {
		s.log().Warn("Last sweep time unreadable; sweeping as if it is due", zap.Error(err))
	}

	if !last.IsZero() {
		s.passed(last)
	}

	first := lastrun.Due(last, s.now(), interval, floor)

	s.log().Info("Lifecycle sweeper started",
		zap.Duration("interval", interval),
		zap.Duration("first_pass", first),
		zap.Time("last_sweep", last),
	)

	timer := time.NewTimer(first)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		timer.Reset(interval)

		start := s.now()

		report, err := s.Sweep(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}

			s.log().Warn("Lifecycle sweep failed; rules apply again on the next pass", zap.Error(err))

			continue
		}

		// Recorded after the pass, so an interrupted one is still due — and
		// recorded even when the pass deleted nothing, because a sweep that
		// found nothing to expire is still a sweep. Skipping it here would
		// leave every quiet deployment re-listing its buckets on each restart.
		done := s.now()
		s.passed(done)

		if err := s.setLastRun(ctx, done); err != nil && ctx.Err() == nil {
			s.log().Warn("Could not record the sweep time; a restart will sweep again", zap.Error(err))
		}

		if report.Expired == 0 && report.Aborted == 0 && report.Noncurrent == 0 && report.Markers == 0 && report.Failed == 0 {
			continue
		}

		s.log().Info("Lifecycle sweep complete",
			zap.Int("buckets", report.Buckets),
			zap.Int("expired", report.Expired),
			zap.Int("noncurrent", report.Noncurrent),
			zap.Int("markers", report.Markers),
			zap.Int("aborted", report.Aborted),
			zap.Int("raced", report.Raced),
			zap.Int("failed_buckets", report.Failed),
			zap.Duration("took", s.now().Sub(start)),
		)
	}
}

// Sweep runs one pass over every bucket that has rules.
//
// A bucket that fails is logged and skipped rather than failing the pass: one
// unreachable bucket must not stop every other bucket's rules from applying,
// and the next pass starts over from what is on disk anyway.
func (s *Sweeper) Sweep(ctx context.Context) (report Report, err error) {
	defer func() { s.record(report) }()

	store, ok := s.Storage.(fs.BucketLifecycleStore)
	if !ok {
		return report, nil
	}

	buckets, err := s.Storage.ListBuckets(ctx)
	if err != nil {
		return report, errors.Wrap(err, "list buckets")
	}

	for _, b := range buckets {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}

		if s.Owns != nil && !s.Owns(b.Name) {
			continue
		}

		rules, err := store.BucketLifecycle(ctx, b.Name)
		if err != nil {
			s.log().Warn("Lifecycle rules unreadable; bucket skipped",
				zap.String("bucket", b.Name), zap.Error(err))

			report.Failed++

			continue
		}

		if !hasEnabledRule(rules) {
			continue
		}

		report.Buckets++

		failed := false

		if err := s.expire(ctx, b.Name, rules, &report); err != nil {
			s.log().Warn("Lifecycle expiry incomplete; resumes on the next pass",
				zap.String("bucket", b.Name), zap.Error(err))

			failed = true
		}

		aborted, err := s.abortUploads(ctx, b.Name, rules)
		report.Aborted += aborted

		if err != nil {
			s.log().Warn("Lifecycle upload cleanup incomplete; resumes on the next pass",
				zap.String("bucket", b.Name), zap.Error(err))

			failed = true
		}

		if failed {
			report.Failed++
		}
	}

	return report, nil
}

// expire applies the bucket's expiration rules. A bucket that has ever been
// versioned is walked version by version, which is one listing that serves the
// rules on current versions, noncurrent ones and delete markers alike; any
// other bucket by its objects.
func (s *Sweeper) expire(ctx context.Context, bucket string, rules []fs.LifecycleRule, report *Report) error {
	if !expires(rules) {
		return nil // Only uploads to abort: no listing needed.
	}

	if v, ok := s.Storage.(fs.Versioner); ok {
		state, err := v.BucketVersioning(ctx, bucket)
		if err != nil {
			return errors.Wrap(err, "read versioning")
		}

		if state != fs.VersioningUnset {
			return s.expireVersions(ctx, v, bucket, rules, report)
		}
	}

	expired, raced, err := s.expireObjects(ctx, bucket, rules)
	report.Expired += expired
	report.Raced += raced

	return err
}

// keyState is what the version walk knows of the key it is in. Versions come
// newest first within a key, and a key may span pages, so it is carried from
// one page to the next.
type keyState struct {
	key string
	// successor is when the previous entry was written, which is when the
	// current one became noncurrent; zero before the key's first entry.
	successor time.Time
	// noncurrent is how many noncurrent versions of the key are newer than
	// the entry at hand.
	noncurrent int
	// marker is the key's current delete marker, decided once the rest of
	// the key shows whether anything is left under it.
	marker *fs.ObjectVersion
	// left reports that a version under the marker stays.
	left bool
}

// expireVersions walks a versioned bucket's versions and applies every
// expiration rule to them.
//
// Removing a noncurrent version by its ID needs no fence: a version ID names
// content that never changes, so a write racing the sweep cannot be the
// version removed. Expiring the current version stays fenced on its ETag, as
// in expireObjects.
func (s *Sweeper) expireVersions(ctx context.Context, v fs.Versioner, bucket string, rules []fs.LifecycleRule, report *Report) error {
	req := fs.ListObjectVersionsRequest{Bucket: bucket, Limit: sweepPage, Tags: hasTags(rules)}

	var k keyState

	for {
		page, err := v.ListObjectVersions(ctx, &req)
		if err != nil {
			return errors.Wrap(err, "list object versions")
		}

		now := s.now()

		for _, ver := range page.Versions {
			if ver.Key != k.key {
				if err := s.settleMarker(ctx, v, bucket, rules, &k, now, report); err != nil {
					return err
				}

				k = keyState{key: ver.Key}
			}

			if err := s.expireVersion(ctx, v, bucket, rules, &k, ver, now, report); err != nil {
				return err
			}
		}

		if !page.IsTruncated {
			return s.settleMarker(ctx, v, bucket, rules, &k, s.now(), report)
		}

		req.KeyMarker, req.VersionIDMarker = page.NextKeyMarker, page.NextVersionIDMarker
	}
}

// expireVersion applies the rules to one entry of the version listing.
func (s *Sweeper) expireVersion(
	ctx context.Context, v fs.Versioner, bucket string, rules []fs.LifecycleRule,
	k *keyState, ver fs.ObjectVersion, now time.Time, report *Report,
) error {
	defer func() { k.successor = ver.LastModified }()

	switch {
	case ver.IsLatest && ver.DeleteMarker:
		k.marker = &ver
	case ver.IsLatest:
		at, rule := fs.LifecycleExpiry(rules, judged(ver), ver.LastModified)
		if at.IsZero() || now.Before(at) {
			return nil
		}

		switch err := s.deleteExpired(ctx, bucket, fs.Object{Key: ver.Key, ETag: ver.ETag}); {
		case err == nil:
			report.Expired++

			// The delete marker it got makes it the newest noncurrent
			// version, which the rules keeping the newest count.
			k.noncurrent++

			s.log().Debug("Lifecycle expired an object",
				zap.String("bucket", bucket),
				zap.String("key", ver.Key),
				zap.String("rule", rule),
			)
		case errors.Is(err, fs.ErrPreconditionFailed):
			report.Raced++
		case errors.Is(err, fs.ErrObjectNotFound):
		default:
			return errors.Wrapf(err, "delete %q", ver.Key)
		}
	default:
		newer := k.noncurrent
		k.noncurrent++

		at, rule := fs.LifecycleNoncurrentExpiry(rules, judged(ver), k.successor, newer)
		if k.successor.IsZero() || at.IsZero() || now.Before(at) {
			k.left = true

			return nil
		}

		switch _, err := v.DeleteObjectVersion(ctx, bucket, ver.Key, ver.VersionID); {
		case err == nil:
			report.Noncurrent++

			s.log().Debug("Lifecycle removed a noncurrent version",
				zap.String("bucket", bucket),
				zap.String("key", ver.Key),
				zap.String("version_id", ver.VersionID),
				zap.String("rule", rule),
			)
		case errors.Is(err, fs.ErrObjectNotFound):
			// Removed by a client since the listing: gone either way.
		default:
			return errors.Wrapf(err, "remove %q version %q", ver.Key, ver.VersionID)
		}
	}

	return nil
}

// settleMarker removes the key's current delete marker if nothing is left
// under it and a rule says so.
//
// The listing still shows the versions this pass removed, which is why the
// walk tracks what is left rather than asking whether the marker was the only
// entry: the marker of a key whose last noncurrent version just went is
// removed in the same pass. A write that lands after the listing makes the
// marker noncurrent, and removing a noncurrent marker changes nothing a reader
// sees.
func (s *Sweeper) settleMarker(
	ctx context.Context, v fs.Versioner, bucket string, rules []fs.LifecycleRule,
	k *keyState, now time.Time, report *Report,
) error {
	if k.marker == nil || k.left {
		return nil
	}

	m := k.marker

	at, rule := fs.LifecycleMarkerExpiry(rules, m.Key, m.LastModified)
	if at.IsZero() || now.Before(at) {
		return nil
	}

	switch _, err := v.DeleteObjectVersion(ctx, bucket, m.Key, m.VersionID); {
	case err == nil:
		report.Markers++

		s.log().Debug("Lifecycle removed an expired delete marker",
			zap.String("bucket", bucket),
			zap.String("key", m.Key),
			zap.String("rule", rule),
		)
	case errors.Is(err, fs.ErrObjectNotFound):
	default:
		return errors.Wrapf(err, "remove %q delete marker", m.Key)
	}

	return nil
}

// expireObjects deletes every object in the bucket whose expiry has passed.
//
// It pages rather than listing the bucket whole, and deleting does not disturb
// the cursor: StartAfter is an exclusive lower bound on the key, so the next
// page resumes after the last key seen whether or not that key still exists.
func (s *Sweeper) expireObjects(ctx context.Context, bucket string, rules []fs.LifecycleRule) (expired, raced int, err error) {
	req := fs.ListObjectsRequest{Bucket: bucket, Limit: sweepPage, Tags: hasTags(rules)}

	for {
		page, err := s.Storage.ListObjects(ctx, &req)
		if err != nil {
			return expired, raced, errors.Wrap(err, "list objects")
		}

		now := s.now()

		for _, obj := range page.Objects {
			at, rule := fs.LifecycleExpiry(rules, fs.LifecycleObject{Key: obj.Key, Size: obj.Size, Tags: obj.Tags}, obj.LastModified)
			if at.IsZero() || now.Before(at) {
				continue
			}

			switch err := s.deleteExpired(ctx, bucket, obj); {
			case err == nil:
				expired++

				s.log().Debug("Lifecycle expired an object",
					zap.String("bucket", bucket),
					zap.String("key", obj.Key),
					zap.String("rule", rule),
				)
			case errors.Is(err, fs.ErrPreconditionFailed):
				raced++
			case errors.Is(err, fs.ErrObjectNotFound):
				// Already gone: a concurrent delete beat the sweep to it,
				// which is the outcome the sweep wanted anyway.
			default:
				return expired, raced, errors.Wrapf(err, "delete %q", obj.Key)
			}
		}

		if !page.IsTruncated {
			return expired, raced, nil
		}

		req.StartAfter = page.NextStartAfter
	}
}

// deleteExpired removes one expired object.
//
// When the backend can delete conditionally, the delete is fenced on the ETag
// the listing saw. Expiry is decided from a listing that is already stale by
// the time the delete runs, and an unfenced delete would destroy the fresh
// object a client wrote in between — data loss caused by the cleanup that was
// supposed to be safe. A backend without the capability keeps the plain
// delete, which is what it had before lifecycle existed.
func (s *Sweeper) deleteExpired(ctx context.Context, bucket string, obj fs.Object) error {
	if cond, ok := s.Storage.(fs.ConditionalDeleter); ok && obj.ETag != "" {
		return cond.DeleteObjectIf(ctx, bucket, obj.Key, fs.Conditions{IfMatch: obj.ETag})
	}

	return s.Storage.DeleteObject(ctx, bucket, obj.Key)
}

// abortUploads aborts every multipart upload the bucket's rules have abandoned.
func (s *Sweeper) abortUploads(ctx context.Context, bucket string, rules []fs.LifecycleRule) (int, error) {
	uploads, err := s.Storage.ListMultipartUploads(ctx, bucket)
	if err != nil {
		return 0, errors.Wrap(err, "list multipart uploads")
	}

	now := s.now()

	var aborted int

	for _, up := range uploads {
		at, rule := fs.LifecycleAbortAt(rules, up.Key, up.Initiated)
		if at.IsZero() || now.Before(at) {
			continue
		}

		switch err := s.Storage.AbortMultipartUpload(ctx, bucket, up.Key, up.UploadID); {
		case err == nil:
			aborted++

			s.log().Debug("Lifecycle aborted an abandoned upload",
				zap.String("bucket", bucket),
				zap.String("key", up.Key),
				zap.String("upload_id", up.UploadID),
				zap.String("rule", rule),
			)
		case errors.Is(err, fs.ErrUploadNotFound):
			// Completed or aborted between the listing and here.
		default:
			return aborted, errors.Wrapf(err, "abort upload %q", up.UploadID)
		}
	}

	return aborted, nil
}

// judged is what the rules judge a listed version by.
func judged(ver fs.ObjectVersion) fs.LifecycleObject {
	return fs.LifecycleObject{Key: ver.Key, Size: ver.Size, Tags: ver.Tags}
}

// hasTags reports whether any enabled rule filters on tags: only then is a
// listing asked for them, so a prefix- or size-only configuration pays
// nothing for tags.
func hasTags(rules []fs.LifecycleRule) bool {
	return slices.ContainsFunc(rules, func(r fs.LifecycleRule) bool { return r.Enabled() && r.HasTags() })
}

// hasEnabledRule reports whether any rule would do something.
func hasEnabledRule(rules []fs.LifecycleRule) bool {
	for _, r := range rules {
		if r.Enabled() {
			return true
		}
	}

	return false
}

// expires reports whether any enabled rule removes objects or versions, as
// opposed to only aborting uploads.
func expires(rules []fs.LifecycleRule) bool {
	for _, r := range rules {
		if r.Enabled() && (r.ExpirationDays > 0 || !r.ExpirationDate.IsZero() || r.Versioned()) {
			return true
		}
	}

	return false
}
