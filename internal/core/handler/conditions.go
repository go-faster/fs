package handler

import (
	"net/http"
	"strconv"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// Conditional-write headers. If-Match and If-None-Match are the standard HTTP
// validators; the size and last-modified forms are S3 extensions that let a
// caller pin a mutation to the exact object it observed without an ETag.
const (
	headerIfMatch                 = "If-Match"
	headerIfNoneMatch             = "If-None-Match"
	headerIfMatchSize             = "x-amz-if-match-size"
	headerIfMatchLastModifiedTime = "x-amz-if-match-last-modified-time"
)

// errMalformedCondition marks a conditional header the server could not parse.
// It maps to InvalidArgument, not to a dropped condition: silently ignoring an
// unparseable condition would turn a guarded mutation into an unguarded one,
// which is the failure mode a conditional request exists to prevent.
var errMalformedCondition = errors.New("malformed conditional header")

// requestConditions extracts the conditional-write headers from r.
func requestConditions(r *http.Request) (fs.Conditions, error) {
	cond := fs.Conditions{
		IfMatch:     r.Header.Get(headerIfMatch),
		IfNoneMatch: r.Header.Get(headerIfNoneMatch),
	}

	if raw := r.Header.Get(headerIfMatchSize); raw != "" {
		size, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || size < 0 {
			return fs.Conditions{}, errors.Wrap(errMalformedCondition, headerIfMatchSize)
		}

		cond.Size = &size
	}

	if raw := r.Header.Get(headerIfMatchLastModifiedTime); raw != "" {
		t, err := http.ParseTime(raw)
		if err != nil {
			return fs.Conditions{}, errors.Wrap(errMalformedCondition, headerIfMatchLastModifiedTime)
		}

		cond.LastModified = &t
	}

	return cond, nil
}

// deleteWithConditions deletes bucket/key, honoring cond when it is set.
//
// Conditions need a backend that evaluates them atomically with the delete
// (fs.ConditionalDeleter). Against one that cannot, the request is refused
// rather than served by a check-then-delete that would race with a concurrent
// write: a condition that is not actually enforced is worse than one that
// fails loudly.
func (h *handler) deleteWithConditions(r *http.Request, bucket, key string, cond fs.Conditions) error {
	if cond.IsZero() {
		return h.service.DeleteObject(r.Context(), bucket, key)
	}

	deleter, ok := h.service.(fs.ConditionalDeleter)
	if !ok {
		return errors.Wrap(fs.ErrUnsupportedOperation, "conditional delete")
	}

	return deleter.DeleteObjectIf(r.Context(), bucket, key, cond)
}
