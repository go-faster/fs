package meta

import "github.com/go-faster/fs/internal/cluster/table"

// BlockRef records that a version references a block. A block whose refs are
// all deleted can be collected.
type BlockRef struct {
	// Deleted only ever turns on: the version is gone, or never completed.
	Deleted bool `json:"deleted,omitempty"`
}

// CompactBlockRef is the block_refs table's compaction: a deleted reference
// goes once every replica holds it.
func CompactBlockRef(r BlockRef) (BlockRef, table.Compaction) {
	if r.Deleted {
		return r, table.Delete
	}

	return r, table.Keep
}

// MergeBlockRef is the block_refs table's merge.
func MergeBlockRef(a, b BlockRef) BlockRef {
	return BlockRef{Deleted: a.Deleted || b.Deleted}
}
