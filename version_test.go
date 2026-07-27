package fs_test

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// TestNewVersionID_SortsNewestFirst is the property the whole read path rests
// on: plain ascending lexical order must put the newest version first, because
// "resolve the current version" is implemented as "take the first entry".
func TestNewVersionID_SortsNewestFirst(t *testing.T) {
	t.Parallel()

	const n = 50

	ids := make([]string, 0, n)

	for range n {
		ids = append(ids, fs.NewVersionID())

		time.Sleep(time.Microsecond)
	}

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)

	// ids is in creation order (oldest first); sorted must be the reverse.
	for i := range ids {
		require.Equalf(t, ids[len(ids)-1-i], sorted[i],
			"position %d: ascending sort must yield newest-first", i)
	}
}

// TestNewVersionID_Format pins the frozen shape. Changing it means migrating
// every stored version, so a failure here is a data-format decision, not a
// test to update.
func TestNewVersionID_Format(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 1000)

	for range 1000 {
		id := fs.NewVersionID()

		require.Len(t, id, 32)
		require.True(t, fs.ValidVersionID(id), "generated ID must validate: %q", id)

		_, dup := seen[id]
		require.False(t, dup, "duplicate version ID %q", id)

		seen[id] = struct{}{}
	}
}

// TestValidVersionID covers the guard between a client's string and a lookup
// key or a filesystem path.
func TestValidVersionID(t *testing.T) {
	t.Parallel()

	valid := []string{fs.NewVersionID(), "null", "0123456789abcdef0123456789abcdef"}
	for _, id := range valid {
		require.Truef(t, fs.ValidVersionID(id), "%q should be valid", id)
	}

	invalid := []string{
		"",
		"NULL",
		"0123456789ABCDEF0123456789ABCDEF",  // uppercase is not what we emit
		"0123456789abcdef0123456789abcde",   // 31
		"0123456789abcdef0123456789abcdef0", // 33
		"../../etc/passwd",
		"0123456789abcdef0123456789abcdeg", // not hex
	}
	for _, id := range invalid {
		require.Falsef(t, fs.ValidVersionID(id), "%q should be rejected", id)
	}
}
