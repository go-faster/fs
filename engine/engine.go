// Package engine is the storage engine: fs.Storage over the cluster's
// replicated metadata tables and content-addressed blocks.
//
// A bucket is a row of the buckets table; its objects are rows of the objects
// table under the bucket's ID, each holding the key's version list. Object
// data is cut into blocks stored by hash, or kept inline in the version when
// it is small. A single node is the same engine over a one-node layout.
//
// Writes to one key are serialized on the node that coordinates them, so a
// condition checked before a write holds when it lands — put-if-absent and
// compare-and-swap are atomic through one node. Across coordinators they are
// not: see internal/cluster/meta.
package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"hash/fnv"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/cluster/peer"
	"github.com/go-faster/fs/internal/cluster/table"
	"github.com/go-faster/fs/internal/sse"
)

// DefaultBlockSize is the size objects are cut into. Larger blocks mean
// fewer files and less metadata per byte on large drives.
const DefaultBlockSize = 1 << 20

// DefaultInlineLimit is the size below which an object is kept in its version
// instead of in blocks: a block costs a file on three nodes and a reference
// row, which a few hundred bytes do not justify.
const DefaultInlineLimit = 3 << 10

// DefaultCodedMinSize is the smallest block an erasure-coded bucket codes:
// below it, K+M shard files cost more I/O than three whole copies save.
const DefaultCodedMinSize = 256 << 10

// Config configures an Engine.
type Config struct {
	Member *peer.Member
	// DB holds this node's replicas of the metadata tables.
	DB *bbolt.DB
	// Blocks stores object data.
	Blocks *block.Manager
	// BlockSize and InlineLimit default to DefaultBlockSize and
	// DefaultInlineLimit.
	BlockSize   int
	InlineLimit int
	// CodedMinSize defaults to DefaultCodedMinSize.
	CodedMinSize int
	// Keyring seals the data keys of encrypted objects. Without one, a
	// request to encrypt is refused rather than stored in the clear.
	Keyring *sse.Keyring
}

// Engine implements fs.Storage.
type Engine struct {
	db      *bbolt.DB
	buckets *table.Table[meta.Bucket]
	objects *table.Table[meta.Object]
	refs    *table.Table[meta.BlockRef]
	parts   *table.Table[meta.LWW[json.RawMessage]]
	uploads *table.Table[meta.LWW[json.RawMessage]]
	blocks  *block.Manager
	member  *peer.Member

	blockSize   int
	inlineLimit int
	codedMin    int
	keyring     *sse.Keyring

	locks [256]sync.Mutex
	now   func() time.Time

	gcMu sync.Mutex
	gc   gcStats

	// bufs recycles block buffers between reads, so a GET does not allocate
	// (and the kernel zero) a fresh block for every block it serves.
	bufs sync.Pool
}

var (
	_ fs.Storage            = (*Engine)(nil)
	_ fs.ConditionalDeleter = (*Engine)(nil)
	_ fs.BucketOwnership    = (*Engine)(nil)
	_ fs.ObjectAttributer   = (*Engine)(nil)
)

// New returns an engine over cfg, creating its tables in cfg.DB and
// registering their peer endpoints; call it before serving.
func New(cfg Config) (*Engine, error) {
	e := &Engine{
		blocks:      cfg.Blocks,
		blockSize:   cmp.Or(cfg.BlockSize, DefaultBlockSize),
		inlineLimit: cmp.Or(cfg.InlineLimit, DefaultInlineLimit),
		codedMin:    cmp.Or(cfg.CodedMinSize, DefaultCodedMinSize),
		member:      cfg.Member,
		keyring:     cfg.Keyring,
		db:          cfg.DB,
		now:         time.Now,
	}

	if e.blockSize > block.MaxSize {
		return nil, errors.Errorf("block size %d exceeds %d", e.blockSize, block.MaxSize)
	}

	var err error

	if e.buckets, err = table.New(meta.Buckets, cfg.DB, cfg.Member, meta.MergeBucket, nil); err != nil {
		return nil, err
	}

	if e.objects, err = table.New(meta.Objects, cfg.DB, cfg.Member, meta.MergeObject, meta.CompactObject); err != nil {
		return nil, err
	}

	if e.refs, err = table.New(meta.BlockRefs, cfg.DB, cfg.Member, meta.MergeBlockRef, meta.CompactBlockRef); err != nil {
		return nil, err
	}

	if e.parts, err = table.New(meta.Parts, cfg.DB, cfg.Member, meta.MergeLWW[json.RawMessage], meta.CompactDone); err != nil {
		return nil, err
	}

	if e.uploads, err = table.New(meta.Uploads, cfg.DB, cfg.Member, meta.MergeLWW[json.RawMessage], meta.CompactDone); err != nil {
		return nil, err
	}

	return e, nil
}

// lock serializes writes to one key on this node.
func (e *Engine) lock(bucketID, key string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucketID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))

	mu := &e.locks[h.Sum32()%uint32(len(e.locks))]
	mu.Lock()

	return mu.Unlock
}

func (e *Engine) ts() int64 { return e.now().UnixNano() }

// newID mints an ID in the S3 version-ID format, which every version, upload
// and bucket incarnation uses alike: 32 hex characters, sortable by time.
func newID() string { return fs.NewVersionID() }

// payload is what an engine version holds beyond what merging needs.
type payload struct {
	Size         int64             `json:"size"`
	ETag         string            `json:"etag,omitempty"`
	LastModified time.Time         `json:"mtime"`
	Meta         fs.ObjectMetadata `json:"meta,omitzero"`
	Owner        fs.Owner          `json:"owner,omitzero"`
	// Inline holds a small object's content; Blocks a larger one's, in order.
	Inline []byte     `json:"inline,omitempty"`
	Blocks []blockLoc `json:"blocks,omitempty"`
	// Parts and UploadID describe a completed multipart object.
	Parts    []fs.ObjectPart `json:"parts,omitempty"`
	UploadID string          `json:"upload_id,omitempty"`
	// Enc is set for an encrypted version; Size is then the plaintext's.
	Enc *encInfo `json:"enc,omitempty"`
	// The client-visible checksum, when one was asked for.
	ChecksumAlgorithm string `json:"cksum_alg,omitempty"`
	Checksum          string `json:"cksum,omitempty"`
	ChecksumType      string `json:"cksum_type,omitempty"`
}

type blockLoc struct {
	Hash block.Hash `json:"h"`
	Size int64      `json:"n"`
	// Scheme is how the block is stored, as block.ParseScheme reads it:
	// empty for replicated.
	Scheme string `json:"sc,omitempty"`
}

// attrs are a version's mutable attributes.
type attrs struct {
	Tags []fs.Tag `json:"tags,omitempty"`
	ACL  fs.ACL   `json:"acl,omitempty"`
}

func decodePayload(v meta.Version) (payload, error) {
	var p payload
	if len(v.Payload) == 0 {
		return p, nil
	}

	if err := json.Unmarshal(v.Payload, &p); err != nil {
		return p, errors.Wrapf(err, "decode version %s", v.ID)
	}

	return p, loadKey(v, p.Enc)
}

// decodeUpload returns an in-flight upload's payload.
func decodeUpload(v meta.Version) (uploadPayload, error) {
	var up uploadPayload
	if err := json.Unmarshal(v.Payload, &up); err != nil {
		return up, errors.Wrapf(err, "decode upload %s", v.ID)
	}

	return up, loadKey(v, up.Enc)
}

func decodeAttrs(v meta.Version) attrs {
	var a attrs

	_ = json.Unmarshal(v.Attrs.V, &a)

	return a
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // Engine types always encode.
	}

	return b
}

// state is what conditional requests are evaluated against.
func state(o meta.Object) (fs.ObjectState, error) {
	v, ok := o.Current()
	if !ok {
		return fs.ObjectState{}, nil
	}

	p, err := decodePayload(v)
	if err != nil {
		return fs.ObjectState{}, err
	}

	return fs.ObjectState{Exists: true, ETag: p.ETag, Size: p.Size, LastModified: p.LastModified}, nil
}

// object reads a key's row; a missing row is an empty one.
func (e *Engine) object(ctx context.Context, bucketID, key string) (meta.Object, error) {
	o, _, err := e.objects.Get(ctx, bucketID, key)

	return o, err
}

// nextTS orders a write after everything o holds.
func (e *Engine) nextTS(o meta.Object) int64 {
	var prev int64
	if v, ok := o.Latest(); ok {
		prev = v.TS
	}

	return meta.NextTS(e.ts(), prev)
}

// release marks every block a version references as no longer referenced by
// it, in one write. A failure leaks the blocks; it never loses data.
func (e *Engine) release(ctx context.Context, owner string, blocks []blockLoc) {
	rows, err := e.releaseBlocks(owner, blocks)
	if err != nil {
		return
	}

	_ = table.Write(ctx, e.member, rows...)
}

// releaseBlocks are the block_refs rows releasing owner's references to
// blocks, for a Write.
func (e *Engine) releaseBlocks(owner string, blocks []blockLoc) ([]table.Row, error) {
	rows := make([]table.Row, 0, len(blocks))

	for _, b := range blocks {
		r, err := e.refs.Row(b.Hash.String(), owner, meta.BlockRef{Deleted: true})
		if err != nil {
			return nil, err
		}

		rows = append(rows, r)
	}

	return rows, nil
}

// releaseReplaced releases the blocks of the versions the merge of next drops
// from before: older null versions a new null version replaces.
func (e *Engine) releaseReplaced(ctx context.Context, before, after meta.Object) {
	for _, v := range before.Versions {
		if _, kept := after.Find(v.ID); kept {
			continue
		}

		if p, err := decodePayload(v); err == nil {
			e.release(ctx, v.ID, p.Blocks)
		}
	}
}

// finishParts marks every part of a completed or aborted upload done, so
// tombstone collection removes them.
func (e *Engine) finishParts(ctx context.Context, uploadID string, parts map[int]storedPart) {
	rows := make([]table.Entry[meta.LWW[json.RawMessage]], 0, len(parts))
	for n := range parts {
		rows = append(rows, table.Entry[meta.LWW[json.RawMessage]]{PK: uploadID, SK: partSK(n), Row: meta.Done})
	}

	_ = e.parts.InsertMany(ctx, uploadID, rows)
}

// row is a table row for writeRows; the error is kept for writeRows to
// report, so a write's rows read as one list.
func row[R any](t *table.Table[R], pk, sk string, r R) rowOrErr {
	tr, err := t.Row(pk, sk, r)

	return rowOrErr{tr, err}
}

type rowOrErr struct {
	row table.Row
	err error
}

// writeRows writes rows of any tables in one commit per node.
func (e *Engine) writeRows(ctx context.Context, rows ...rowOrErr) error {
	out := make([]table.Row, 0, len(rows))

	for _, r := range rows {
		if r.err != nil {
			return r.err
		}

		out = append(out, r.row)
	}

	return table.Write(ctx, e.member, out...)
}

// releaseRows are the block_refs rows that release the blocks of the
// versions merging after drops from before, for a Write with the change.
func (e *Engine) releaseRows(before, after meta.Object) ([]table.Row, error) {
	var rows []table.Row

	for _, v := range before.Versions {
		if _, kept := after.Find(v.ID); kept {
			continue
		}

		p, err := decodePayload(v)
		if err != nil {
			continue // A payload that does not decode references nothing we can name.
		}

		released, err := e.releaseBlocks(v.ID, p.Blocks)
		if err != nil {
			return nil, err
		}

		rows = append(rows, released...)
	}

	return rows, nil
}

// BlockLive reports whether any version still references h, for block GC.
func (e *Engine) BlockLive(ctx context.Context, h block.Hash) (bool, error) {
	refs, err := e.refs.Range(ctx, h.String(), "", 1<<20)
	if err != nil {
		return false, err
	}

	for _, r := range refs {
		if !r.Row.Deleted {
			return true, nil
		}
	}

	return false, nil
}
