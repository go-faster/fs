// Package meta defines the rows of the cluster's metadata tables and how
// replicas merge them.
//
// Every merge here is commutative, associative and idempotent — the contract
// internal/cluster/table relies on — so replicas converge whatever order and
// however many times writes reach them. The types carry what merging needs;
// what an object version holds beyond that (headers, checksums, where its
// bytes live) is the storage engine's payload, opaque to the merge.
//
// Tables and keys:
//
//   - buckets: one partition (partition key ""), sort key the bucket name, so
//     listing buckets is one scan.
//   - objects: partition key the bucket's ID, sort key the object key. A
//     bucket's listing is one scan of one partition; a recreated bucket gets a
//     fresh ID and starts empty.
//   - block_refs: partition key the block hash, sort key the version ID that
//     references it.
//
// Two limits follow from that and are by design, as in Garage: one bucket's
// objects live on one partition's replicas, so a bucket is bounded by what one
// node's metadata store holds (on the order of 1e9 objects); and conditions
// evaluated before a write — put-if-absent, If-Match, deleting only an empty
// bucket — see the merged row a quorum returned, not a lock, so two
// coordinators can both pass the check. The later version wins the merge;
// neither write is lost or torn.
package meta

import (
	"bytes"
	"encoding/json"
)

// Table names.
const (
	Buckets   = "buckets"
	Objects   = "objects"
	BlockRefs = "block_refs"
	// Parts holds multipart upload parts: partition key the upload's version
	// ID, sort key the zero-padded part number, an LWW register per part so a
	// re-uploaded part replaces the earlier one.
	Parts = "parts"
)

// LWW is a last-writer-wins register.
type LWW[T any] struct {
	// TS orders writes; see NextTS.
	TS int64 `json:"ts"`
	V  T     `json:"v"`
}

// Merge keeps the later write. Equal timestamps — two coordinators writing in
// the same nanosecond — are ordered by encoded value, so every replica keeps
// the same one.
func (a LWW[T]) Merge(b LWW[T]) LWW[T] {
	switch {
	case b.TS > a.TS:
		return b
	case b.TS < a.TS:
		return a
	}

	if bytes.Compare(encode(b.V), encode(a.V)) > 0 {
		return b
	}

	return a
}

// MergeLWW is the merge of a table whose rows are single registers.
func MergeLWW[T any](a, b LWW[T]) LWW[T] { return a.Merge(b) }

// NextTS is the timestamp for a write to a row last written at prev: the
// coordinator's clock, but never at or before prev. A coordinator whose clock
// runs behind still orders its write after the one it read.
func NextTS(now, prev int64) int64 {
	return max(now, prev+1)
}

func encode(v any) []byte {
	b, _ := json.Marshal(v)

	return b
}
