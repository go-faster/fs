package engine

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/cluster/table"
	"github.com/go-faster/fs/internal/sse"
)

// The target side of geo-replication, and the export its source makes. A
// replicated version arrives as state, not as an operation: the target
// merges it into the key's row like any other write of that version, so
// delivering it twice, late or out of order changes nothing, and a version
// the target deleted stays deleted.
//
// Blocks are claimed before they are sent: ClaimReplicaBlocks references
// each from the version it belongs to, the reference the version itself
// will hold, so collection keeps a block from its claim on, however long
// the rest of the shipment takes. A version that never arrives leaves its
// claims, and the blocks they hold, behind.

var _ fs.ReplicaReceiver = (*Engine)(nil)

// ReplicaInfo implements fs.ReplicaReceiver.
func (e *Engine) ReplicaInfo(ctx context.Context, bucket string) (fs.ReplicaInfo, error) {
	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return fs.ReplicaInfo{}, err
	}

	return fs.ReplicaInfo{
		Protocol:   fs.ReplicaProtocol,
		Versioning: versioning(b),
		Encryption: e.keyring != nil,
	}, nil
}

// replicaScheme is how this deployment stores a block of size bytes of b:
// as its new blocks are.
func (e *Engine) replicaScheme(b meta.Bucket, size int64) string {
	if sch := bucketScheme(b); sch.Coded() && size >= int64(e.codedMin) {
		return sch.String()
	}

	return ""
}

// ClaimReplicaBlocks implements fs.ReplicaReceiver. Only a replicated block
// can be found already stored; an erasure-coded one is always asked for.
func (e *Engine) ClaimReplicaBlocks(
	ctx context.Context, bucket, versionID string, blocks []fs.ReplicaBlock,
) ([]fs.ReplicaBlockClaim, error) {
	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return nil, err
	}

	if !validReplicaID(versionID) {
		return nil, errors.Errorf("invalid version ID %q", versionID)
	}

	rows := make([]table.Row, 0, len(blocks))
	locs := make([]blockLoc, 0, len(blocks))

	for _, rb := range blocks {
		h, err := block.ParseHash(rb.Hash)
		if err != nil {
			return nil, err
		}

		if rb.Size <= 0 || rb.Size > block.MaxSize {
			return nil, errors.Errorf("block %s: size %d", rb.Hash, rb.Size)
		}

		r, err := e.refs.Row(h.String(), versionID, meta.BlockRef{})
		if err != nil {
			return nil, err
		}

		rows = append(rows, r)
		locs = append(locs, blockLoc{Hash: h, Size: rb.Size, Scheme: e.replicaScheme(b, rb.Size)})
	}

	// The references first: from here on collection keeps the blocks.
	if err := table.Write(ctx, e.member, rows...); err != nil {
		return nil, err
	}

	out := make([]fs.ReplicaBlockClaim, len(locs))

	for i, loc := range locs {
		out[i] = fs.ReplicaBlockClaim{Hash: loc.Hash.String(), Scheme: loc.Scheme, Missing: true}

		if loc.Scheme != "" {
			continue
		}

		has, err := e.blocks.Has(ctx, loc.Hash)
		if err != nil {
			return nil, err
		}

		out[i].Missing = !has
	}

	return out, nil
}

// PutReplicaBlock implements fs.ReplicaReceiver.
func (e *Engine) PutReplicaBlock(ctx context.Context, bucket, versionID string, rb fs.ReplicaBlock, data []byte) error {
	if _, _, err := e.bucket(ctx, bucket); err != nil {
		return err
	}

	if !validReplicaID(versionID) {
		return errors.Errorf("invalid version ID %q", versionID)
	}

	h, err := block.ParseHash(rb.Hash)
	if err != nil {
		return err
	}

	if block.Sum(data) != h {
		return errors.Wrapf(fs.ErrBadDigest, "block %s: content does not match", rb.Hash)
	}

	if rb.Scheme != "" {
		if _, err := block.ParseScheme(rb.Scheme); err != nil {
			return err
		}
	}

	return e.storeBlock(ctx, versionID, blockLoc{Hash: h, Size: int64(len(data)), Scheme: rb.Scheme}, data, nil)
}

// validReplicaID reports whether id can be a version's own ID.
func validReplicaID(id string) bool { return id != fs.NullVersionID && fs.ValidVersionID(id) }

// ReplicaStats count the versions this deployment received from others.
type ReplicaStats struct {
	// Applied versions include those that changed nothing: delivered again,
	// or deleted here already.
	Applied, Refused int64
}

// ReplicaStats returns the counts of versions received since start.
func (e *Engine) ReplicaStats() ReplicaStats {
	return ReplicaStats{Applied: e.replicasApplied.Load(), Refused: e.replicasRefused.Load()}
}

// ApplyReplicas implements fs.ReplicaReceiver.
func (e *Engine) ApplyReplicas(ctx context.Context, bucket string, versions []fs.ReplicaVersion) ([]fs.ReplicaResult, error) {
	b, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return nil, err
	}

	if versioning(b) != fs.VersioningEnabled {
		return nil, errors.Wrap(fs.ErrInvalidBucketState, "a replication target needs versioning enabled")
	}

	out := make([]fs.ReplicaResult, len(versions))

	for i, rv := range versions {
		out[i].VersionID = rv.VersionID

		if err := e.applyReplica(ctx, inc.ID, rv); err != nil {
			// A failure to write is the deployment's, not the version's:
			// the whole request fails, and the source retries it.
			if errors.Is(err, fs.ErrUnavailable) {
				return nil, err
			}

			out[i].Error = err.Error()

			e.replicasRefused.Add(1)

			continue
		}

		e.replicasApplied.Add(1)
	}

	return out, nil
}

// applyReplica merges one replicated version into its key's row.
func (e *Engine) applyReplica(ctx context.Context, bucketID string, rv fs.ReplicaVersion) error {
	if !validReplicaID(rv.VersionID) {
		return errors.Errorf("invalid version ID %q", rv.VersionID)
	}

	if rv.Key == "" || rv.TS <= 0 {
		return errors.New("no key or timestamp")
	}

	defer e.lock(bucketID, rv.Key)()

	before, err := e.object(ctx, bucketID, rv.Key)
	if err != nil {
		return err
	}

	held, ok := before.Find(rv.VersionID)

	attrsReg := meta.LWW[json.RawMessage]{TS: rv.AttrsTS, V: mustJSON(attrs{Tags: rv.Tags, ACL: rv.ACL})}

	if rv.AttrsOnly {
		if !ok || held.State != meta.Complete {
			return nil // Nothing to change: an attributes change never creates a version.
		}

		held.Attrs = attrsReg

		return e.objects.Insert(ctx, bucketID, rv.Key, meta.Object{Versions: []meta.Version{held}})
	}

	if ok && held.State == meta.Gone {
		return nil // Deleted here; the merge would keep it deleted.
	}

	v, locs, err := e.replicaVersion(ctx, rv)
	if err != nil {
		return err
	}

	v.Attrs = attrsReg
	row := meta.Object{Versions: []meta.Version{v}}

	version, err := e.objects.Row(bucketID, rv.Key, row)
	if err != nil {
		return err
	}

	rows := []table.Row{version}

	for _, loc := range locs {
		r, err := e.refs.Row(loc.Hash.String(), v.ID, meta.BlockRef{})
		if err != nil {
			return err
		}

		rows = append(rows, r)
	}

	released, unref, err := e.releaseRows(before, meta.MergeObject(before, row))
	if err != nil {
		return err
	}

	// No outbox row: a replica is not replicated on. No quota either: the
	// source acknowledged the write already, and refusing it here would only
	// stall the queue behind it.
	if err := table.Write(ctx, e.member, slices.Concat(rows, released)...); err != nil {
		return err
	}

	e.unreferenced(ctx, unref)

	e.charge(bucketID, before, row)

	return nil
}

// replicaVersion is rv as a version of this deployment, and the blocks it
// references, which must be stored here.
func (e *Engine) replicaVersion(ctx context.Context, rv fs.ReplicaVersion) (meta.Version, []blockLoc, error) {
	v := meta.Version{
		ID:           rv.VersionID,
		TS:           rv.TS,
		Null:         rv.Null,
		State:        meta.Complete,
		DeleteMarker: rv.DeleteMarker,
	}

	if rv.DeleteMarker {
		return v, nil, nil
	}

	p := payload{
		Size:              rv.Size,
		ETag:              rv.ETag,
		LastModified:      rv.LastModified,
		Meta:              rv.Metadata,
		Owner:             rv.Owner,
		Inline:            rv.Inline,
		Parts:             rv.Parts,
		UploadID:          rv.UploadID,
		ChecksumAlgorithm: rv.ChecksumAlgorithm,
		Checksum:          rv.Checksum,
		ChecksumType:      rv.ChecksumType,
		Replica:           true,
	}

	for _, rb := range rv.Blocks {
		h, err := block.ParseHash(rb.Hash)
		if err != nil {
			return v, nil, err
		}

		if rb.Scheme != "" {
			if _, err := block.ParseScheme(rb.Scheme); err != nil {
				return v, nil, err
			}
		} else {
			// A coded block was stored here by PutReplicaBlock; a replicated
			// one may have been found already stored, and is checked.
			has, err := e.blocks.Has(ctx, h)
			if err != nil {
				return v, nil, err
			}

			if !has {
				return v, nil, errors.Errorf("block %s is not stored", rb.Hash)
			}
		}

		p.Blocks = append(p.Blocks, blockLoc{Hash: h, Size: rb.Size, Scheme: rb.Scheme})
	}

	if enc := rv.Encryption; enc != nil {
		var key json.RawMessage

		switch {
		case enc.Customer:
			var w sse.WrappedKey
			if err := json.Unmarshal(enc.WrappedKey, &w); err != nil {
				return v, nil, errors.Wrap(err, "customer-sealed data key")
			}

			key = mustJSON(w)
		case e.keyring == nil:
			return v, nil, errors.New("the version is encrypted and no master key is configured here")
		default:
			w, err := e.keyring.Wrap(enc.DataKey)
			if err != nil {
				return v, nil, errors.Wrap(err, "wrap data key")
			}

			key = mustJSON(w)
		}

		p.Enc = &encInfo{NonceBase: enc.NonceBase, Customer: enc.Customer}
		v.Key = meta.LWW[json.RawMessage]{TS: rv.TS, V: key}
	}

	v.Payload = mustJSON(p)

	return v, p.Blocks, nil
}

// ExportReplica returns a version of key as it is replicated: whole, or its
// tags and ACL alone. versionID is the version's own ID, "null" never.
// A version that is not complete — deleted, or an upload — is
// fs.ErrVersionNotFound.
func (e *Engine) ExportReplica(ctx context.Context, bucket, key, versionID string, attrsOnly bool) (fs.ReplicaVersion, error) {
	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return fs.ReplicaVersion{}, err
	}

	o, err := e.object(ctx, inc.ID, key)
	if err != nil {
		return fs.ReplicaVersion{}, err
	}

	v, ok := o.Find(versionID)
	if !ok || v.State != meta.Complete {
		return fs.ReplicaVersion{}, fs.ErrVersionNotFound
	}

	a := decodeAttrs(v)
	rv := fs.ReplicaVersion{
		Key:          key,
		VersionID:    v.ID,
		TS:           v.TS,
		Null:         v.Null,
		AttrsOnly:    attrsOnly,
		DeleteMarker: v.DeleteMarker,
		Tags:         a.Tags,
		ACL:          a.ACL,
		AttrsTS:      v.Attrs.TS,
	}

	if attrsOnly || v.DeleteMarker {
		return rv, nil
	}

	p, err := decodePayload(v)
	if err != nil {
		return fs.ReplicaVersion{}, err
	}

	rv.Size, rv.ETag, rv.LastModified = p.Size, p.ETag, p.LastModified
	rv.Metadata, rv.Owner = p.Meta, p.Owner
	rv.Inline, rv.Parts, rv.UploadID = p.Inline, p.Parts, p.UploadID
	rv.ChecksumAlgorithm, rv.Checksum, rv.ChecksumType = p.ChecksumAlgorithm, p.Checksum, p.ChecksumType

	for _, b := range p.Blocks {
		rv.Blocks = append(rv.Blocks, fs.ReplicaBlock{Hash: b.Hash.String(), Size: b.Size, Scheme: b.Scheme})
	}

	if p.Enc != nil {
		if err := loadKey(v, p.Enc); err != nil {
			return fs.ReplicaVersion{}, err
		}

		enc := &fs.ReplicaEncryption{NonceBase: p.Enc.NonceBase, Customer: p.Enc.Customer}

		switch {
		case p.Enc.Customer:
			enc.WrappedKey = mustJSON(p.Enc.Key)
		case e.keyring == nil:
			return fs.ReplicaVersion{}, errors.Errorf("version %s is encrypted and no master key is configured", v.ID)
		default:
			dek, err := e.keyring.Unwrap(p.Enc.Key)
			if err != nil {
				return fs.ReplicaVersion{}, errors.Wrapf(err, "unwrap data key of %s", v.ID)
			}

			enc.DataKey = dek
		}

		rv.Encryption = enc
	}

	return rv, nil
}

// ReadReplicaBlock returns a block of an exported version, as stored here.
func (e *Engine) ReadReplicaBlock(ctx context.Context, rb fs.ReplicaBlock) ([]byte, error) {
	h, err := block.ParseHash(rb.Hash)
	if err != nil {
		return nil, err
	}

	return e.getBlock(ctx, blockLoc{Hash: h, Size: rb.Size, Scheme: rb.Scheme}, nil)
}
