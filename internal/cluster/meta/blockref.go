package meta

// BlockRef records that a version references a block. A block whose refs are
// all deleted can be collected.
type BlockRef struct {
	// Deleted only ever turns on: the version is gone, or never completed.
	Deleted bool `json:"deleted,omitempty"`
}

// MergeBlockRef is the block_refs table's merge.
func MergeBlockRef(a, b BlockRef) BlockRef {
	return BlockRef{Deleted: a.Deleted || b.Deleted}
}
