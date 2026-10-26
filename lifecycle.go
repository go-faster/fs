package fs

import (
	"context"
	"slices"
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
// to the subset this server enforces: expire objects selected by prefix, tags
// and size after N days, remove noncurrent versions N days after they were replaced, remove
// delete markers left with nothing under them, and abort multipart uploads
// left unfinished for N days.
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
	// Tags restricts the rule to versions carrying every one of these tags,
	// with these values. Empty matches any tags.
	Tags []Tag
	// SizeGreaterThan restricts the rule to versions of more than this many
	// bytes. Nil sets no lower bound; 0 excludes empty objects.
	SizeGreaterThan *int64
	// SizeLessThan restricts the rule to versions of fewer than this many
	// bytes. Nil sets no upper bound.
	SizeLessThan *int64
	// ExpirationDays deletes a matching object this many days after its last
	// modification. Zero means the rule expires nothing by age.
	ExpirationDays int
	// ExpirationDate deletes every matching object once this instant passes,
	// whenever they were written. Zero means the rule expires nothing by date.
	ExpirationDate time.Time
	// AbortIncompleteMultipartUploadDays aborts a matching multipart upload
	// this many days after it was initiated. Zero means the rule aborts none.
	AbortIncompleteMultipartUploadDays int
	// NoncurrentDays removes a noncurrent version this many days after it
	// became noncurrent. Zero means the rule removes no noncurrent version.
	NoncurrentDays int
	// NewerNoncurrentVersions keeps this many of a key's newest noncurrent
	// versions whatever their age. Zero keeps none.
	NewerNoncurrentVersions int
	// ExpiredObjectDeleteMarker removes a key's delete marker once it is the
	// key's only version.
	ExpiredObjectDeleteMarker bool
}

// Enabled reports whether the rule applies.
func (r LifecycleRule) Enabled() bool { return r.Status == LifecycleEnabled }

// LifecycleObject is what a rule judges a version by. A delete marker is
// zero bytes with no tags.
type LifecycleObject struct {
	Key  string
	Size int64
	Tags []Tag
}

// Matches reports whether the rule covers o: every predicate of its filter
// holds.
func (r LifecycleRule) Matches(o LifecycleObject) bool {
	if !strings.HasPrefix(o.Key, r.Prefix) {
		return false
	}

	if r.SizeGreaterThan != nil && o.Size <= *r.SizeGreaterThan {
		return false
	}

	if r.SizeLessThan != nil && o.Size >= *r.SizeLessThan {
		return false
	}

	for _, want := range r.Tags {
		if !slices.Contains(o.Tags, want) {
			return false
		}
	}

	return true
}

// MatchesUpload reports whether the rule covers a multipart upload in
// progress of key. An upload has neither tags nor a size yet, so only the
// prefix applies; the S3 layer refuses an abort rule that filters on either.
func (r LifecycleRule) MatchesUpload(key string) bool { return strings.HasPrefix(key, r.Prefix) }

// HasTags reports whether the rule filters on tags, which a sweep can only
// judge from a listing that carries them.
func (r LifecycleRule) HasTags() bool { return len(r.Tags) > 0 }

// HasSize reports whether the rule filters on size.
func (r LifecycleRule) HasSize() bool { return r.SizeGreaterThan != nil || r.SizeLessThan != nil }

// Versioned reports whether a rule acts on versions: noncurrent ones, or
// delete markers. Only a backend that can list and remove versions can
// enforce it.
func (r LifecycleRule) Versioned() bool {
	return r.NoncurrentDays > 0 || r.ExpiredObjectDeleteMarker
}

// LifecycleExpiry returns when o, last modified at modified, expires under
// rules, and the ID of the rule that decides it. A zero time means no enabled
// rule expires it.
//
// When more than one rule matches, the earliest expiry wins: S3 has no rule
// precedence, and the alternative — keeping an object a later rule allows —
// would leave data alive that a rule the client wrote says should be gone.
func LifecycleExpiry(rules []LifecycleRule, o LifecycleObject, modified time.Time) (at time.Time, ruleID string) {
	for _, r := range rules {
		if !r.Enabled() || !r.Matches(o) {
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

// LifecycleNoncurrentExpiry returns when o, a noncurrent version, is removed
// under rules, and the ID of the rule that decides it. since is when
// it became noncurrent, which is when its successor was written, and newer is
// how many noncurrent versions of the key are newer than it. A zero time means
// no enabled rule removes it.
//
// The earliest removal wins, as in LifecycleExpiry.
func LifecycleNoncurrentExpiry(rules []LifecycleRule, o LifecycleObject, since time.Time, newer int) (at time.Time, ruleID string) {
	for _, r := range rules {
		if !r.Enabled() || r.NoncurrentDays <= 0 || !r.Matches(o) {
			continue
		}

		// The rule keeps the key's NewerNoncurrentVersions newest noncurrent
		// versions, so a version is removable once that many are newer.
		if newer < r.NewerNoncurrentVersions {
			continue
		}

		when := lifecycleExpiryAt(since, r.NoncurrentDays)
		if at.IsZero() || when.Before(at) {
			at, ruleID = when, r.ID
		}
	}

	return at, ruleID
}

// LifecycleMarkerExpiry returns when an expired object delete marker of key
// (a delete marker that is the key's only version), written at modified, is
// removed under rules, and the ID of the rule that decides it. A zero time
// means no enabled rule removes it. The marker is judged as zero bytes with
// no tags, so no tag rule removes it.
//
// ExpiredObjectDeleteMarker removes it at once. An Expiration Days rule
// removes it too, once it is as old as the rule's objects get: S3 does that
// cleanup without being asked.
func LifecycleMarkerExpiry(rules []LifecycleRule, key string, modified time.Time) (at time.Time, ruleID string) {
	marker := LifecycleObject{Key: key}

	for _, r := range rules {
		if !r.Enabled() || !r.Matches(marker) {
			continue
		}

		var when time.Time

		switch {
		case r.ExpiredObjectDeleteMarker:
			when = modified
		case r.ExpirationDays > 0:
			when = lifecycleExpiryAt(modified, r.ExpirationDays)
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
		if !r.Enabled() || r.AbortIncompleteMultipartUploadDays <= 0 || !r.MatchesUpload(key) {
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
