package engine

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/meta"
)

// written is content stored for a version: inline, or as blocks.
type written struct {
	size   int64
	etag   string
	inline []byte
	blocks []blockLoc
}

// write stores r's content: inline when it fits, else as blocks referenced by
// the version owner. References are written before the blocks are, so a block
// is never stored unreferenced past its write; a failure leaves references to
// blocks that may not exist, which GC treats as live until released.
func (e *Engine) write(ctx context.Context, r io.Reader, owner string) (written, error) {
	h := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.
	buf := make([]byte, e.blockSize)

	var w written

	for {
		n, err := io.ReadFull(io.TeeReader(r, h), buf)
		if n > 0 {
			chunk := bytes.Clone(buf[:n])
			w.size += int64(n)

			// Only a first chunk that already reached the end can be inline:
			// the whole object is in it.
			last := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
			if len(w.blocks) == 0 && last && n <= e.inlineLimit {
				w.inline = chunk
			} else if err := e.putBlock(ctx, owner, chunk, &w); err != nil {
				return w, err
			}
		}

		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}

		if err != nil {
			e.release(ctx, owner, w.blocks)

			return w, errors.Wrap(err, "read object")
		}
	}

	if w.size == 0 {
		w.inline = nil
	}

	w.etag = hex.EncodeToString(h.Sum(nil))

	return w, nil
}

func (e *Engine) putBlock(ctx context.Context, owner string, data []byte, w *written) error {
	hash := block.Sum(data)

	if err := e.refs.Insert(ctx, hash.String(), owner, meta.BlockRef{}); err != nil {
		e.release(ctx, owner, w.blocks)

		return err
	}

	if _, err := e.blocks.Put(ctx, data); err != nil {
		e.release(ctx, owner, append(w.blocks, blockLoc{Hash: hash}))

		return err
	}

	w.blocks = append(w.blocks, blockLoc{Hash: hash, Size: int64(len(data))})

	return nil
}

func refuseEncryption(algorithm string) error {
	if algorithm == "" {
		return nil
	}

	return errors.Wrapf(fs.ErrUnsupportedOperation, "server-side encryption (%s) is not supported yet by this engine", algorithm)
}

func (e *Engine) PutObject(ctx context.Context, req *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
	if err := refuseEncryption(req.ServerSideEncryption); err != nil {
		return nil, err
	}

	b, inc, err := e.bucket(ctx, req.Bucket)
	if err != nil {
		return nil, err
	}

	versioned := versioning(b) == fs.VersioningEnabled
	cond := req.Conditions()

	// A condition that already fails is refused before the body is stored.
	if !cond.IsZero() {
		o, err := e.object(ctx, inc.ID, req.Key)
		if err != nil {
			return nil, err
		}

		st, err := state(o)
		if err != nil {
			return nil, err
		}

		if err := cond.CheckWrite(st); err != nil {
			return nil, err
		}
	}

	id := newID()

	w, err := e.write(ctx, req.Reader, id)
	if err != nil {
		return nil, err
	}

	if req.ContentMD5 != "" && req.ContentMD5 != w.etag {
		e.release(ctx, id, w.blocks)

		return nil, fs.ErrBadDigest
	}

	p := payload{
		Size:   w.size,
		ETag:   w.etag,
		Meta:   req.Metadata,
		Owner:  req.Owner,
		Inline: w.inline,
		Blocks: w.blocks,
	}

	if err := e.commit(ctx, inc.ID, req.Key, id, !versioned, cond, p, attrs{Tags: req.Tags, ACL: req.ACL}); err != nil {
		e.release(ctx, id, w.blocks)

		return nil, err
	}

	resp := &fs.PutObjectResponse{ETag: w.etag}
	if versioned {
		resp.VersionID = id
	}

	return resp, nil
}

// commit makes the version current: under the key's lock, it checks cond
// against the row as a quorum has it and writes the completed version — a null
// one, replacing the key's older null versions, unless versioning is enabled.
func (e *Engine) commit(
	ctx context.Context, bucketID, key, id string, null bool, cond fs.Conditions, p payload, a attrs,
) error {
	defer e.lock(bucketID, key)()

	before, err := e.object(ctx, bucketID, key)
	if err != nil {
		return err
	}

	st, err := state(before)
	if err != nil {
		return err
	}

	if err := cond.CheckWrite(st); err != nil {
		return err
	}

	ts := e.nextTS(before)
	p.LastModified = e.now().UTC()

	v := meta.Version{
		ID:      id,
		TS:      ts,
		Null:    null,
		State:   meta.Complete,
		Payload: mustJSON(p),
		Attrs:   meta.LWW[json.RawMessage]{TS: ts, V: mustJSON(a)},
	}

	row := meta.Object{Versions: []meta.Version{v}}
	if err := e.objects.Insert(ctx, bucketID, key, row); err != nil {
		return err
	}

	e.releaseReplaced(ctx, before, meta.MergeObject(before, row))

	return nil
}

// current returns the key's current version and its payload.
func (e *Engine) current(ctx context.Context, bucket, key string) (meta.Version, payload, error) {
	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return meta.Version{}, payload{}, err
	}

	o, err := e.object(ctx, inc.ID, key)
	if err != nil {
		return meta.Version{}, payload{}, err
	}

	v, ok := o.Current()
	if !ok {
		return v, payload{}, fs.ErrObjectNotFound
	}

	p, err := decodePayload(v)

	return v, p, err
}

func (e *Engine) GetObject(ctx context.Context, bucket, key string) (*fs.GetObjectResponse, error) {
	v, _, err := e.current(ctx, bucket, key)
	if err != nil {
		return nil, err
	}

	return e.response(ctx, v)
}

func (e *Engine) DeleteObject(ctx context.Context, bucket, key string) error {
	return e.DeleteObjectIf(ctx, bucket, key, fs.Conditions{})
}

// DeleteObjectIf implements fs.ConditionalDeleter. On a bucket that was never
// versioned it removes the key's null version outright; once versioning has
// been configured it is DeleteObjectVersionIf without a version ID, which
// writes a delete marker.
func (e *Engine) DeleteObjectIf(ctx context.Context, bucket, key string, cond fs.Conditions) error {
	b, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return err
	}

	if versioning(b) != fs.VersioningUnset {
		_, err := e.DeleteObjectVersionIf(ctx, bucket, key, "", cond)

		return err
	}

	defer e.lock(inc.ID, key)()

	before, err := e.object(ctx, inc.ID, key)
	if err != nil {
		return err
	}

	st, err := state(before)
	if err != nil {
		return err
	}

	if err := cond.CheckDelete(st); err != nil {
		return err
	}

	cur, ok := before.Current()
	if !ok {
		return fs.ErrObjectNotFound
	}

	_, err = e.removeVersion(ctx, inc.ID, key, before, s3ID(cur))

	return err
}

// ListObjects streams the bucket's rows in key order, folding by delimiter as
// it goes and seeking past a folded prefix instead of reading it — the same
// page FoldPage would build, without holding the bucket in memory.
func (e *Engine) ListObjects(ctx context.Context, req *fs.ListObjectsRequest) (*fs.ListObjectsResponse, error) {
	_, inc, err := e.bucket(ctx, req.Bucket)
	if err != nil {
		return nil, err
	}

	const page = 1000

	out := &fs.ListObjectsResponse{}
	full := func() bool { return req.Limit > 0 && len(out.Objects)+len(out.CommonPrefixes) >= req.Limit }

	// Keys at or before StartAfter are skipped below; starting there only
	// saves reading them.
	start := max(req.Prefix, req.StartAfter)

	for {
		rows, err := e.objects.Range(ctx, inc.ID, start, page)
		if err != nil {
			return nil, err
		}

		next, jumped := "", false

		for _, r := range rows {
			if !strings.HasPrefix(r.SK, req.Prefix) {
				return out, nil // Keys sort after the prefix: nothing more matches.
			}

			if r.SK <= req.StartAfter {
				continue
			}

			v, ok := r.Row.Current()
			if !ok {
				continue // Deleted, or only uploads in flight.
			}

			entry, folded := r.SK, false

			if req.Delimiter != "" {
				if i := strings.Index(r.SK[len(req.Prefix):], req.Delimiter); i >= 0 {
					entry, folded = r.SK[:len(req.Prefix)+i+len(req.Delimiter)], true
				}
			}

			if folded && entry <= req.StartAfter {
				// A prefix an earlier page returned: skip everything under it.
				next, jumped = prefixEnd(entry), true

				break
			}

			if full() {
				out.IsTruncated = true

				return out, nil
			}

			out.NextStartAfter = entry

			if folded {
				out.CommonPrefixes = append(out.CommonPrefixes, entry)
				next, jumped = prefixEnd(entry), true

				break
			}

			p, err := decodePayload(v)
			if err != nil {
				return nil, err
			}

			out.Objects = append(out.Objects, fs.Object{
				Key:          r.SK,
				Size:         p.Size,
				LastModified: p.LastModified,
				ETag:         p.ETag,
				Owner:        p.Owner,
			})
		}

		switch {
		case jumped:
			start = next
		case len(rows) < page:
			return out, nil
		default:
			start = rows[len(rows)-1].SK + "\x00"
		}
	}
}

// prefixEnd is the smallest key after every key that starts with p.
func prefixEnd(p string) string {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++

			return string(b[:i+1])
		}
	}

	return p + "\xff" // All 0xff: no key with this prefix sorts after it.
}

// updateAttrs rewrites the current version's mutable attributes.
func (e *Engine) updateAttrs(ctx context.Context, bucket, key string, fn func(*attrs)) error {
	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return err
	}

	defer e.lock(inc.ID, key)()

	o, err := e.object(ctx, inc.ID, key)
	if err != nil {
		return err
	}

	v, ok := o.Current()
	if !ok {
		return fs.ErrObjectNotFound
	}

	a := decodeAttrs(v)
	fn(&a)

	v.Attrs = meta.LWW[json.RawMessage]{TS: meta.NextTS(e.ts(), v.Attrs.TS), V: mustJSON(a)}

	return e.objects.Insert(ctx, inc.ID, key, meta.Object{Versions: []meta.Version{v}})
}

func (e *Engine) GetObjectTagging(ctx context.Context, bucket, key string) ([]fs.Tag, error) {
	v, _, err := e.current(ctx, bucket, key)
	if err != nil {
		return nil, err
	}

	return decodeAttrs(v).Tags, nil
}

func (e *Engine) PutObjectTagging(ctx context.Context, bucket, key string, tags []fs.Tag) error {
	return e.updateAttrs(ctx, bucket, key, func(a *attrs) { a.Tags = tags })
}

func (e *Engine) DeleteObjectTagging(ctx context.Context, bucket, key string) error {
	return e.updateAttrs(ctx, bucket, key, func(a *attrs) { a.Tags = nil })
}

func (e *Engine) ObjectACL(ctx context.Context, bucket, key string) (fs.ACL, error) {
	v, _, err := e.current(ctx, bucket, key)
	if err != nil {
		return fs.ACLPrivate, err
	}

	return normalizeACL(decodeAttrs(v).ACL), nil
}

func (e *Engine) SetObjectACL(ctx context.Context, bucket, key string, acl fs.ACL) error {
	return e.updateAttrs(ctx, bucket, key, func(a *attrs) { a.ACL = acl })
}

func (e *Engine) ObjectOwner(ctx context.Context, bucket, key string) (fs.Owner, error) {
	_, p, err := e.current(ctx, bucket, key)
	if err != nil {
		return fs.Owner{}, err
	}

	return p.Owner, nil
}

// ObjectAttributes implements fs.ObjectAttributer.
func (e *Engine) ObjectAttributes(ctx context.Context, bucket, key string) (*fs.ObjectAttributes, error) {
	_, p, err := e.current(ctx, bucket, key)
	if err != nil {
		return nil, err
	}

	return &fs.ObjectAttributes{
		ETag:         p.ETag,
		Size:         p.Size,
		LastModified: p.LastModified,
		Parts:        p.Parts,
		UploadID:     p.UploadID,
	}, nil
}
