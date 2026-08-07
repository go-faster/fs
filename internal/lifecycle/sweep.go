// Package lifecycle enforces bucket lifecycle rules: it deletes objects that
// have expired and aborts multipart uploads that were abandoned.
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
	"time"

	"github.com/go-faster/errors"
	"go.uber.org/zap"

	"github.com/go-faster/fs"
)

// sweepPage is how many keys a listing pass asks for at a time. The sweep walks
// whole buckets, so it pages rather than asking for everything: a bucket is
// unbounded and a pass must not need it in memory.
const sweepPage = 1000

// defaultFirstPass is how long Run waits before its first sweep.
//
// Not the interval, and not zero. Waiting a full interval means a node
// restarted more often than the interval never sweeps at all — deploy hourly
// with a 12h interval and lifecycle rules silently stop applying, which is the
// failure this whole feature exists to avoid. Sweeping the instant the process
// is up is the other extreme: a crashlooping node would re-list every bucket on
// every restart, piling load on exactly when it is least able to carry it.
//
// A short delay makes a healthy restart sweep promptly and a fast crashloop
// never get there.
//
// ponytail: a node restarted more often than this still never sweeps. The fix
// is durable state — persist when the last pass ran and sweep on start when it
// is older than the interval — which is worth it only once someone has a
// deployment that restarts every few minutes.
const defaultFirstPass = 5 * time.Minute

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
	// FirstPass is how long Run waits before its first sweep; zero means
	// defaultFirstPass, and anything longer than the interval is capped to it.
	FirstPass time.Duration
}

// Report is what one pass did.
type Report struct {
	// Buckets is how many buckets carried enabled rules.
	Buckets int
	// Expired and Aborted are objects deleted and uploads aborted.
	Expired, Aborted int
	// Raced is objects that expired by the listing but changed before the
	// delete, and so were left alone. They expire on a later pass if the write
	// that overtook them did not reset their age past the rule.
	Raced int
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

// Run sweeps shortly after starting and every interval thereafter, until ctx is
// canceled. A non-positive interval disables enforcement, which is why the S3
// layer must not accept rules when the sweep is off: stored rules nothing
// enforces are a lie to the client.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	first := s.FirstPass
	if first <= 0 {
		first = defaultFirstPass
	}

	if first > interval {
		first = interval
	}

	s.log().Info("Lifecycle sweeper started",
		zap.Duration("interval", interval),
		zap.Duration("first_pass", first),
	)

	// A timer rather than a ticker, so the first pass is not a full interval
	// away — see defaultFirstPass.
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

		if report.Expired == 0 && report.Aborted == 0 {
			continue
		}

		s.log().Info("Lifecycle sweep complete",
			zap.Int("buckets", report.Buckets),
			zap.Int("expired", report.Expired),
			zap.Int("aborted", report.Aborted),
			zap.Int("raced", report.Raced),
			zap.Duration("took", s.now().Sub(start)),
		)
	}
}

// Sweep runs one pass over every bucket that has rules.
//
// A bucket that fails is logged and skipped rather than failing the pass: one
// unreachable bucket must not stop every other bucket's rules from applying,
// and the next pass starts over from what is on disk anyway.
func (s *Sweeper) Sweep(ctx context.Context) (Report, error) {
	var report Report

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

		rules, err := store.BucketLifecycle(ctx, b.Name)
		if err != nil {
			s.log().Warn("Lifecycle rules unreadable; bucket skipped",
				zap.String("bucket", b.Name), zap.Error(err))

			continue
		}

		if !hasEnabledRule(rules) {
			continue
		}

		report.Buckets++

		expired, raced, err := s.expireObjects(ctx, b.Name, rules)
		report.Expired += expired
		report.Raced += raced

		if err != nil {
			s.log().Warn("Lifecycle expiry incomplete; resumes on the next pass",
				zap.String("bucket", b.Name), zap.Error(err))
		}

		aborted, err := s.abortUploads(ctx, b.Name, rules)
		report.Aborted += aborted

		if err != nil {
			s.log().Warn("Lifecycle upload cleanup incomplete; resumes on the next pass",
				zap.String("bucket", b.Name), zap.Error(err))
		}
	}

	return report, nil
}

// expireObjects deletes every object in the bucket whose expiry has passed.
//
// It pages rather than listing the bucket whole, and deleting does not disturb
// the cursor: StartAfter is an exclusive lower bound on the key, so the next
// page resumes after the last key seen whether or not that key still exists.
func (s *Sweeper) expireObjects(ctx context.Context, bucket string, rules []fs.LifecycleRule) (expired, raced int, err error) {
	req := fs.ListObjectsRequest{Bucket: bucket, Limit: sweepPage}

	for {
		page, err := s.Storage.ListObjects(ctx, &req)
		if err != nil {
			return expired, raced, errors.Wrap(err, "list objects")
		}

		now := s.now()

		for _, obj := range page.Objects {
			at, rule := fs.LifecycleExpiry(rules, obj.Key, obj.LastModified)
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

// hasEnabledRule reports whether any rule would do something.
func hasEnabledRule(rules []fs.LifecycleRule) bool {
	for _, r := range rules {
		if r.Enabled() {
			return true
		}
	}

	return false
}
