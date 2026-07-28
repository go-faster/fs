package keyspace_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/metastore/keyspace"
)

// TestSeparatorRemovesAmbiguity is why there is a separator at all. Without a
// byte that can appear in neither half, bucket "a" key "b" and bucket "ab" key
// "" encode identically — one object silently overwriting another.
func TestSeparatorRemovesAmbiguity(t *testing.T) {
	assert.NotEqual(t,
		keyspace.ObjectKey("a", "b"),
		keyspace.ObjectKey("ab", ""),
		"the bucket/key boundary must be unambiguous")
}

// TestBucketsDoNotBleed: a bucket whose name prefixes another's must not have
// its keys land inside the other's range. This is the case a naive separator —
// or none — gets wrong, and it produces a listing that returns another
// bucket's objects.
func TestBucketsDoNotBleed(t *testing.T) {
	inside := keyspace.ObjectKey("photos", "a.jpg")
	outside := keyspace.ObjectKey("photos-archive", "a.jpg")

	lower := keyspace.BucketPrefix("photos")
	upper := keyspace.UpperBound(lower)

	assert.True(t, bytes.Compare(inside, lower) >= 0 && bytes.Compare(inside, upper) < 0,
		"the bucket's own key is inside its range")
	assert.False(t, bytes.Compare(outside, lower) >= 0 && bytes.Compare(outside, upper) < 0,
		"a bucket whose name merely starts the same is outside it")
}

// TestByteOrderIsListingOrder is the property everything above rests on: a
// bucket's keys come out of a range scan already in the order S3 lists them,
// so a listing needs no secondary index and no re-sorting.
func TestByteOrderIsListingOrder(t *testing.T) {
	// Deliberately mixed case and punctuation: byte order puts every uppercase
	// letter before every lowercase one, and a language collation does not.
	keys := []string{"A", "B", "Z", "a", "a-1", "a/b", "a1", "z"}

	encoded := make([][]byte, 0, len(keys))
	for _, k := range keys {
		encoded = append(encoded, keyspace.ObjectKey("photos", k))
	}

	sorted := slices.Clone(encoded)
	slices.SortFunc(sorted, bytes.Compare)

	assert.Equal(t, encoded, sorted, "encoded keys must already be in key order")
}

// TestPrefixesDoNotOverlap: one byte each, so a scan of one kind never sees
// another. Entries, counters and a store's own bookkeeping share a keyspace.
func TestPrefixesDoNotOverlap(t *testing.T) {
	object := keyspace.ObjectKey("photos", "a.jpg")
	usage := keyspace.UsageKey("photos")

	assert.NotEqual(t, object[0], usage[0])
	assert.NotEqual(t, object[0], byte(keyspace.Meta))
	assert.NotEqual(t, usage[0], byte(keyspace.Meta))

	// A scan bounded to the object prefix cannot reach the usage rows.
	lower := []byte{keyspace.Object}
	upper := keyspace.UpperBound(lower)
	assert.Negative(t, bytes.Compare(object, upper))
	assert.GreaterOrEqual(t, bytes.Compare(usage, upper), 0)
}

func TestUpperBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []byte
		want   []byte
	}{
		{"increments the last byte", []byte("abc"), []byte("abd")},
		{"carries past 0xFF", []byte{'a', 0xFF}, []byte{'b'}},
		{"empty has no bound", nil, nil},
		{"all 0xFF has no successor", []byte{0xFF, 0xFF}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, keyspace.UpperBound(tc.prefix))
		})
	}
}

// TestUpperBoundDoesNotMutateItsInput: the bounds are computed from keys the
// caller still holds, and incrementing one in place would corrupt the very key
// the scan was positioned on.
func TestUpperBoundDoesNotMutateItsInput(t *testing.T) {
	prefix := keyspace.BucketPrefix("photos")
	before := slices.Clone(prefix)

	keyspace.UpperBound(prefix)

	require.Equal(t, before, prefix)
}
