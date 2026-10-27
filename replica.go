package fs

import (
	"context"
	"encoding/json"
	"time"
)

// Geo-replication (docs/design/geo-replication.md) moves object versions
// between fs deployments as the types below, which are also the JSON of the
// ?x-fs-replica protocol.

// ReplicaProtocol is the version of the replication protocol this server
// speaks.
const ReplicaProtocol = 1

// ReplicaInfo is what a replication target reports about its bucket, for the
// source to check before it ships anything.
type ReplicaInfo struct {
	Protocol   int             `json:"protocol"`
	Versioning VersioningState `json:"versioning"`
	// Encryption reports a master key configured: without one, versions
	// encrypted with SSE-S3 are refused.
	Encryption bool `json:"encryption"`
}

// ReplicaBlock is one block of a replicated version: its SHA-256 (hex) and
// size, and how it is stored. Scheme is the storing side's: the source's
// when exported, the target's once it has claimed the block.
type ReplicaBlock struct {
	Hash   string `json:"hash"`
	Size   int64  `json:"size"`
	Scheme string `json:"scheme,omitempty"`
}

// ReplicaBlockClaim is a target's answer for one block of a version: how it
// stores the block, and whether it needs the bytes.
type ReplicaBlockClaim struct {
	Hash    string `json:"hash"`
	Scheme  string `json:"scheme,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// ReplicaEncryption is how a replicated version is encrypted. Its blocks and
// inline content are ciphertext either way.
type ReplicaEncryption struct {
	NonceBase []byte `json:"nonce_base"`
	// DataKey is an SSE-S3 version's data key in the clear: master keys are
	// never shared between deployments, so the source unwraps it and the
	// target wraps it with its own.
	DataKey []byte `json:"data_key,omitempty"`
	// Customer marks SSE-C. WrappedKey is the data key as the client's key
	// sealed it, which neither side can open.
	Customer   bool            `json:"customer,omitempty"`
	WrappedKey json.RawMessage `json:"wrapped_key,omitempty"`
}

// ReplicaVersion is one object version as it is replicated.
type ReplicaVersion struct {
	Key string `json:"key"`
	// VersionID is the version's ID, never "null": a null version is Null
	// with its own ID.
	VersionID string `json:"version_id"`
	// TS orders the key's versions, the same on every deployment.
	TS   int64 `json:"ts"`
	Null bool  `json:"null,omitempty"`
	// AttrsOnly carries a change of the version's tags and ACL alone. It
	// applies to a version the target holds, and never creates one.
	AttrsOnly    bool `json:"attrs_only,omitempty"`
	DeleteMarker bool `json:"delete_marker,omitempty"`

	Size              int64          `json:"size,omitempty"`
	ETag              string         `json:"etag,omitempty"`
	LastModified      time.Time      `json:"last_modified,omitzero"`
	Metadata          ObjectMetadata `json:"metadata,omitzero"`
	Owner             Owner          `json:"owner,omitzero"`
	Inline            []byte         `json:"inline,omitempty"`
	Blocks            []ReplicaBlock `json:"blocks,omitempty"`
	Parts             []ObjectPart   `json:"parts,omitempty"`
	UploadID          string         `json:"upload_id,omitempty"`
	ChecksumAlgorithm string         `json:"checksum_algorithm,omitempty"`
	Checksum          string         `json:"checksum,omitempty"`
	ChecksumType      string         `json:"checksum_type,omitempty"`

	Encryption *ReplicaEncryption `json:"encryption,omitempty"`

	Tags []Tag `json:"tags,omitempty"`
	ACL  ACL   `json:"acl,omitempty"`
	// AttrsTS orders changes of the tags and ACL.
	AttrsTS int64 `json:"attrs_ts"`
}

// ReplicaResult is a target's answer for one version: applied when Error is
// empty.
type ReplicaResult struct {
	VersionID string `json:"version_id"`
	Error     string `json:"error,omitempty"`
}

// ReplicaReceiver is a backend that accepts versions replicated from another
// deployment, the target side of geo-replication.
type ReplicaReceiver interface {
	// ReplicaInfo describes bucket as a replication target.
	ReplicaInfo(ctx context.Context, bucket string) (ReplicaInfo, error)
	// ClaimReplicaBlocks references each block from the version it belongs
	// to, so collection keeps it until the version arrives, and answers
	// which the target lacks.
	ClaimReplicaBlocks(ctx context.Context, bucket, versionID string, blocks []ReplicaBlock) ([]ReplicaBlockClaim, error)
	// PutReplicaBlock stores a block of version as its claim said; data
	// must hash to b.Hash.
	PutReplicaBlock(ctx context.Context, bucket, versionID string, b ReplicaBlock, data []byte) error
	// ApplyReplicas merges versions into the bucket, each on its own: one
	// refused does not stop the others.
	ApplyReplicas(ctx context.Context, bucket string, versions []ReplicaVersion) ([]ReplicaResult, error)
}
