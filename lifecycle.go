package fs

import (
	"context"
	"strings"
	"time"
)

// LifecycleDay is how long a lifecycle "day" lasts.
const LifecycleDay = 24 * time.Hour

// Lifecycle rule statuses. A disabled rule is stored and returned unchanged and
// does nothing, which is how S3 lets a client park a rule without deleting it.
const (
	LifecycleEnabled  = "Enabled"
	LifecycleDisabled = "Disabled"
)

// LifecycleRule is one rule of a bucket's lifecycle configuration, restricted
// to the subset this server enforces: expire objects under a prefix after N
// days, and abort multipart uploads left unfinished for N days.
//
// The subset is deliberate and the S3 layer refuses anything outside it rather
// than storing it. A rule the server keeps but never acts on tells a client its
// objects expire when they do not, and a client that believes data is being
// deleted is the one failure mode worth refusing outright.
type LifecycleRule struct {
	// ID names the rule; S3 generates one when the client does not supply it.
	ID string
	// Status is LifecycleEnabled or LifecycleDisabled.
	Status string
	// Prefix restricts the rule to keys beneath it; empty covers the bucket.
	Prefix string
	// ExpirationDays deletes a matching object this many days after its last
	// modification. Zero means the rule expires nothing by age.
	ExpirationDays int
	// ExpirationDate deletes every matching object once this instant passes,
	// whenever they were written. Zero means the rule expires nothing by date.
	ExpirationDate time.Time
	// AbortIncompleteMultipartUploadDays aborts a matching multipart upload
	// this many days after it was initiated. Zero means the rule aborts none.
	AbortIncompleteMultipartUploadDays int
}

// Enabled reports whether the rule applies.
func (r LifecycleRule) Enabled() bool { return r.Status == LifecycleEnabled }

// Matches reports whether the rule covers key.
func (r LifecycleRule) Matches(key string) bool { return strings.HasPrefix(key, r.Prefix) }

// LifecycleExpiry returns when key, last modified at modified, expires under
// rules, and the ID of the rule that decides it. A zero time means no enabled
// rule expires it.
//
// When more than one rule matches, the earliest expiry wins: S3 has no rule
// precedence, and the alternative — keeping an object a later rule allows —
// would leave data alive that a rule the client wrote says should be gone.
func LifecycleExpiry(rules []LifecycleRule, key string, modified time.Time) (at time.Time, ruleID string) {
	for _, r := range rules {
		if !r.Enabled() || !r.Matches(key) {
			continue
		}

		var when time.Time

		switch {
		case r.ExpirationDays > 0:
			when = lifecycleExpiryAt(modified, r.ExpirationDays)
		case !r.ExpirationDate.IsZero():
			when = r.ExpirationDate
		default:
			continue
		}

		if at.IsZero() || when.Before(at) {
			at, ruleID = when, r.ID
		}
	}

	return at, ruleID
}

// lifecycleExpiryAt is when an object last modified at modified expires under a
// Days rule.
//
// S3 does not expire at modification time plus N×24h: it rounds up to the UTC
// midnight following it, so every object written on the same day expires
// together, and the expiry a client is told matches the one it observes.
func lifecycleExpiryAt(modified time.Time, days int) time.Time {
	at := modified.Add(time.Duration(days) * LifecycleDay).UTC()

	return at.Truncate(LifecycleDay).Add(LifecycleDay)
}

// LifecycleAbortAt is when a multipart upload initiated at started is abandoned
// under rules, and the ID of the rule that decides it. A zero time means no
// enabled rule aborts it.
func LifecycleAbortAt(rules []LifecycleRule, key string, started time.Time) (at time.Time, ruleID string) {
	for _, r := range rules {
		if !r.Enabled() || r.AbortIncompleteMultipartUploadDays <= 0 || !r.Matches(key) {
			continue
		}

		when := started.Add(time.Duration(r.AbortIncompleteMultipartUploadDays) * LifecycleDay)
		if at.IsZero() || when.Before(at) {
			at, ruleID = when, r.ID
		}
	}

	return at, ruleID
}

// BucketLifecycleStore is the optional capability of storing a bucket's
// lifecycle rules, which is what the ?lifecycle subresource reads and writes.
//
// Storing rules is only half of the feature: the other half is the sweep that
// enforces them (internal/lifecycle). A backend implements this interface when
// it can persist the rules; the sweep runs over the fs.Storage interface and so
// works against any backend that does.
//
// BucketLifecycle returns nil rules when the bucket has no configuration, which
// the S3 layer reports as NoSuchLifecycleConfiguration.
type BucketLifecycleStore interface {
	BucketLifecycle(ctx context.Context, bucket string) ([]LifecycleRule, error)
	SetBucketLifecycle(ctx context.Context, bucket string, rules []LifecycleRule) error
	DeleteBucketLifecycle(ctx context.Context, bucket string) error
}
