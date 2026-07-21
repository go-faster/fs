package fs

import "strings"

// PreconditionFailed reports whether the request's If-None-Match / If-Match
// conditions fail against the current object state, where exists reports whether
// the target object is present and currentETag is its ETag (quoted or bare;
// only meaningful when exists is true). A true result means the write must be
// rejected with ErrPreconditionFailed.
//
// Storage backends MUST call this while holding the lock that serializes writes
// to the key, so the evaluation is atomic with the write. Evaluating the
// condition in a separate step before the write (check-then-act) races: several
// concurrent If-None-Match: * writers can all observe "absent" and all succeed.
//
// Semantics (matching S3):
//   - If-None-Match: *          fail if the object exists.
//   - If-None-Match: "<etag>"   fail if it exists and the ETag matches.
//   - If-Match: *               fail if the object does not exist.
//   - If-Match: "<etag>"        fail if it is missing or the ETag differs.
func (r *PutObjectRequest) PreconditionFailed(exists bool, currentETag string) bool {
	ifNoneMatch := strings.TrimSpace(r.IfNoneMatch)
	ifMatch := strings.TrimSpace(r.IfMatch)

	if ifNoneMatch == "" && ifMatch == "" {
		return false
	}

	if ifNoneMatch == "*" && exists {
		return true
	}

	if ifNoneMatch != "" && ifNoneMatch != "*" && exists && etagInList(ifNoneMatch, currentETag) {
		return true
	}

	if ifMatch == "*" && !exists {
		return true
	}

	if ifMatch != "" && ifMatch != "*" && (!exists || !etagInList(ifMatch, currentETag)) {
		return true
	}

	return false
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
