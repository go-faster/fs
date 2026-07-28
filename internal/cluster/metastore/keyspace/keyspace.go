// Package keyspace is how the metadata plane lays out its keys.
//
// It exists because more than one thing now depends on the layout and they
// must agree exactly. The node-local pebble store encodes entries with it; the
// sharded plane partitions *this* space into ranges, so a range boundary is a
// position in it; and a listing walks it forward. Two encodings that differed
// by a byte would put a key in one place and look for it in another, and
// nothing would report an error — the object would simply not be found.
//
// # Byte order is listing order
//
// An object key is
//
//	'o' + bucket + 0x00 + key
//
// so a bucket's objects are contiguous and sorted by key in plain byte order,
// which is the order S3 specifies for listings. That is what lets a listing be
// a forward range scan with no secondary index and no re-sorting, and what lets
// a range be nothing more than an interval.
//
// NUL separates because no S3 bucket name may contain one and no object key
// survives XML with one — the same reasoning the coordinator's object
// references already rest on. Without a separator that cannot appear in either
// half, bucket "a" key "b" and bucket "ab" key "" would encode identically.
package keyspace

import "bytes"

// Prefixes. One byte each, so a scan of one kind never sees another.
const (
	// Object prefixes one entry per object.
	Object = 'o'
	// Usage prefixes one row of maintained counters per bucket.
	Usage = 'u'
	// Meta prefixes a store's own bookkeeping — its state and the shape its
	// entries were written in.
	Meta = 'm'
)

// ObjectKey is 'o' + bucket + NUL + key.
func ObjectKey(bucket, key string) []byte {
	out := make([]byte, 0, 2+len(bucket)+len(key))
	out = append(out, Object)
	out = append(out, bucket...)
	out = append(out, 0)
	out = append(out, key...)

	return out
}

// UsageKey is 'u' + bucket.
func UsageKey(bucket string) []byte {
	out := make([]byte, 0, 1+len(bucket))
	out = append(out, Usage)
	out = append(out, bucket...)

	return out
}

// BucketPrefix is every object key of a bucket: 'o' + bucket + NUL.
func BucketPrefix(bucket string) []byte {
	out := make([]byte, 0, 2+len(bucket))
	out = append(out, Object)
	out = append(out, bucket...)
	out = append(out, 0)

	return out
}

// UpperBound is the exclusive end of a prefix scan: the prefix with its last
// byte incremented.
//
// A prefix of all 0xFF has no successor, and nil then means "to the end",
// which is correct because nothing sorts above it.
func UpperBound(prefix []byte) []byte {
	out := bytes.Clone(prefix)

	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xFF {
			out[i]++

			return out[:i+1]
		}
	}

	return nil
}
