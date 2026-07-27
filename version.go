package fs

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// NewVersionID returns a fresh version identifier.
//
// # The format is frozen
//
// 32 lowercase hex characters: the first 16 are the bitwise complement of the
// creation time in unix nanoseconds, the last 16 are random.
//
// Complementing the timestamp is what makes plain ascending lexical order put
// the *newest* version first — which is the order every read wants, because
// resolving "the current version" is then "take the first entry", and a
// directory listing or a sorted key range already arrives that way. No index,
// no cached pointer, no separate ordering column.
//
// The random half breaks ties between versions created in the same
// nanosecond and keeps IDs unguessable, so one version ID does not let a
// caller derive its neighbors.
//
// This format cannot change once written. Every stored version is named by it
// and every listing sorts on it, so a later change means migrating live data —
// the exact trap SeaweedFS fell into by shipping IDs that did not sort and
// then having to convert them mid-flight. It is fixed here deliberately, at
// the cost of getting it right before the first release rather than after.
func NewVersionID() string {
	var id [16]byte

	// ^uint64: newest sorts first. Nanosecond resolution keeps the ordering
	// meaningful for writes to the same key in quick succession, which is
	// exactly when ordering matters.
	binary.BigEndian.PutUint64(id[:8], ^uint64(time.Now().UnixNano())) //nolint:gosec // Complement of a positive time.

	if _, err := rand.Read(id[8:]); err != nil {
		// crypto/rand does not fail in practice; if it ever does, a
		// timestamp-only ID is still unique per nanosecond and correctly
		// ordered, which is what the read path depends on.
		binary.BigEndian.PutUint64(id[8:], uint64(time.Now().UnixNano())) //nolint:gosec // Fallback only.
	}

	return hex.EncodeToString(id[:])
}

// ValidVersionID reports whether s is a version identifier this server could
// have produced, or the reserved "null".
//
// Callers send version IDs back to us, so this is the guard between a client's
// string and a filesystem path or a lookup key.
func ValidVersionID(s string) bool {
	if s == NullVersionID {
		return true
	}

	if len(s) != 32 {
		return false
	}

	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}
