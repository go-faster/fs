package fs

import (
	"strings"
	"time"
)

// ObjectState is the current state of a key, as seen by a backend holding the
// lock that serializes writes to it. It is what conditional requests are
// evaluated against.
type ObjectState struct {
	// Exists reports whether the object is present. The other fields are
	// meaningful only when it is true.
	Exists bool
	// ETag is the stored ETag, quoted or bare.
	ETag         string
	Size         int64
	LastModified time.Time
}

// Conditions carries the conditional headers a mutating request may specify.
// The zero value imposes no condition.
//
// Backends MUST evaluate them while holding the lock that serializes writes to
// the key, so the evaluation is atomic with the mutation. Evaluating the
// condition in a separate step before the write (check-then-act) races:
// several concurrent If-None-Match: * writers can all observe "absent" and all
// succeed.
type Conditions struct {
	// IfMatch and IfNoneMatch are the raw header values: "*" or a
	// comma-separated entity-tag list.
	IfMatch     string
	IfNoneMatch string
	// Size, when set, requires the object to be exactly this many bytes
	// (x-amz-if-match-size).
	Size *int64
	// LastModified, when set, requires the object's modification time to match
	// to the second (x-amz-if-match-last-modified-time).
	LastModified *time.Time
}

// IsZero reports whether no condition is set.
func (c Conditions) IsZero() bool {
	return c.IfMatch == "" && c.IfNoneMatch == "" && c.Size == nil && c.LastModified == nil
}

// CheckWrite evaluates the conditions for a write (PUT, multipart completion)
// against state. It returns:
//
//   - ErrObjectNotFound when If-Match is set and the object is absent. S3
//     reports a conditional write against a missing key as 404 NoSuchKey, not
//     as a failed precondition.
//   - ErrPreconditionFailed when the object is present and a condition does
//     not hold.
//   - nil when the write may proceed.
func (c Conditions) CheckWrite(state ObjectState) error {
	if c.IsZero() {
		return nil
	}

	if strings.TrimSpace(c.IfMatch) != "" && !state.Exists {
		return ErrObjectNotFound
	}

	if !c.holds(state) {
		return ErrPreconditionFailed
	}

	return nil
}

// CheckDelete evaluates the conditions for a delete against state.
//
// Deletion is idempotent: a condition against a key that is already gone is
// not a failure, so an absent object always passes. A present object that does
// not satisfy a condition yields ErrPreconditionFailed.
func (c Conditions) CheckDelete(state ObjectState) error {
	if c.IsZero() || !state.Exists {
		return nil
	}

	if !c.holds(state) {
		return ErrPreconditionFailed
	}

	return nil
}

// holds reports whether every set condition is satisfied by state. An absent
// object satisfies If-None-Match and fails everything else.
func (c Conditions) holds(state ObjectState) bool {
	if ifNoneMatch := strings.TrimSpace(c.IfNoneMatch); ifNoneMatch != "" {
		if ifNoneMatch == "*" && state.Exists {
			return false
		}

		if ifNoneMatch != "*" && state.Exists && etagInList(ifNoneMatch, state.ETag) {
			return false
		}
	}

	if ifMatch := strings.TrimSpace(c.IfMatch); ifMatch != "" {
		if !state.Exists {
			return false
		}

		if ifMatch != "*" && !etagInList(ifMatch, state.ETag) {
			return false
		}
	}

	if c.Size != nil && (!state.Exists || state.Size != *c.Size) {
		return false
	}

	if c.LastModified != nil {
		if !state.Exists ||
			!state.LastModified.Truncate(time.Second).Equal(c.LastModified.Truncate(time.Second)) {
			return false
		}
	}

	return true
}

// Conditions returns the conditional-write headers carried by the request.
func (r *PutObjectRequest) Conditions() Conditions {
	return Conditions{IfMatch: r.IfMatch, IfNoneMatch: r.IfNoneMatch}
}

// PreconditionFailed reports whether the request's If-None-Match / If-Match
// conditions fail against the current object state, where exists reports
// whether the target object is present and currentETag is its ETag (quoted or
// bare; only meaningful when exists is true).
//
// Deprecated: use Conditions().CheckWrite, which distinguishes a failed
// precondition from a conditional write against a missing key — S3 reports the
// latter as 404 NoSuchKey, not 412. Kept for backends outside this repository.
func (r *PutObjectRequest) PreconditionFailed(exists bool, currentETag string) bool {
	return r.Conditions().CheckWrite(ObjectState{Exists: exists, ETag: currentETag}) != nil
}

// etagInList reports whether the raw ETag matches any entity-tag in a
// comma-separated If-Match / If-None-Match header value, tolerating quotes and
// the weak-validator prefix (W/).
func etagInList(header, raw string) bool {
	raw = strings.Trim(raw, `"`)

	for tok := range strings.SplitSeq(header, ",") {
		tok = strings.TrimSpace(tok)
		tok = strings.TrimPrefix(tok, "W/")

		if strings.Trim(tok, `"`) == raw {
			return true
		}
	}

	return false
}
