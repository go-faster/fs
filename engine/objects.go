package engine

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/checksum"
	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/sse"
)

// written is content stored for a version: inline, or as blocks.
type written struct {
	// size and etag describe the plaintext; inline and blocks hold what is
	// stored, which is ciphertext when the content was sealed.
	size   int64
	etag   string
	inline []byte
	blocks []blockLoc
}

// write stores r's content, sealed under c when it is not nil: inline when it
// fits, else as blocks referenced by the version owner. References are
// written before the blocks are, so a block is never stored unreferenced past
// its write; a failure leaves references to blocks that may not exist, which
// GC treats as live until released.
//
// cks, when not nil, is fed the plaintext alongside the ETag's hash.
func (e *Engine) write(ctx context.Context, r io.Reader, owner string, c *sse.Cipher, cks io.Writer) (written, error) {
	h := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.

	var sink io.Writer = h
	if cks != nil {
		sink = io.MultiWriter(h, cks)
	}

	plain := &counter{r: io.TeeReader(r, sink)}

	stored := sealed(plain, c)
	defer func() { _ = stored.Close() }()

	// Blocks are stored concurrently, a few at a time, while the next ones
	// are read: a block's hash and its writes on the replicas overlap with
	// cutting the next, instead of the stream waiting on each in turn.
	var (
		w      written
		sizes  []int64
		mu     sync.Mutex
		hashes = map[int]block.Hash{}
		failed error
		wg     sync.WaitGroup
		sem    = make(chan struct{}, inflight)
	)

	fail := func() error {
		mu.Lock()
		defer mu.Unlock()

		return failed
	}

	for {
		// Each block is read into its own pooled buffer, owned by the store
		// until every replica has it, then returned for the next block.
		bufp := e.buffer()
		chunk := (*bufp)[:e.blockSize]

		n, err := io.ReadFull(stored, chunk)
		chunk = chunk[:max(n, 0)]

		if n == 0 {
			e.bufs.Put(bufp)
		}

		if n > 0 {
			// Only a first chunk that already reached the end can be inline:
			// the whole object is in it. The version keeps it, so it is
			// copied out of the pooled buffer.
			last := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
			if len(sizes) == 0 && last && n <= e.inlineLimit {
				w.inline = bytes.Clone(chunk)

				e.bufs.Put(bufp)
			} else {
				idx := len(sizes)
				sizes = append(sizes, int64(n))

				sem <- struct{}{}

				wg.Go(func() {
					defer func() { <-sem }()

					h := block.Sum(chunk)
					err := e.storeBlock(ctx, owner, h, chunk, func() { e.bufs.Put(bufp) })

					mu.Lock()
					hashes[idx] = h

					if err != nil && failed == nil {
						failed = err
					}
					mu.Unlock()
				})
			}
		}

		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}

		if err == nil {
			err = fail()
		}

		if err != nil {
			wg.Wait()
			e.release(ctx, owner, locs(sizes, hashes))

			return w, errors.Wrap(err, "write object")
		}
	}

	wg.Wait()

	w.blocks = locs(sizes, hashes)

	if err := fail(); err != nil {
		e.release(ctx, owner, w.blocks)

		return w, err
	}

	if len(w.blocks) == 0 && len(w.inline) == 0 {
		w.inline = nil
	}

	w.size = plain.n
	w.etag = hex.EncodeToString(h.Sum(nil))

	return w, nil
}

// inflight bounds the blocks of one object being stored at once.
const inflight = 4

// locs assembles the stored blocks in order.
func locs(sizes []int64, hashes map[int]block.Hash) []blockLoc {
	out := make([]blockLoc, len(sizes))
	for i, n := range sizes {
		out[i] = blockLoc{Hash: hashes[i], Size: n}
	}

	return out
}

// counter counts the bytes read through it.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)

	return n, err
}

// storeBlock references a block from owner, then stores it. The reference
// comes first, so GC never finds the block unreferenced. release, when not
// nil, is called once nothing reads data any more.
func (e *Engine) storeBlock(ctx context.Context, owner string, h block.Hash, data []byte, release func()) error {
	if err := e.refs.Insert(ctx, h.String(), owner, meta.BlockRef{}); err != nil {
		if release != nil {
			release()
		}

		return err
	}

	if err := e.blocks.PutHashed(ctx, h, data, release); err != nil {
		return err
	}

	return nil
}

// buffer returns a pooled buffer large enough for a block and its trailer.
func (e *Engine) buffer() *[]byte {
	if p, ok := e.bufs.Get().(*[]byte); ok && cap(*p) >= e.blockSize+4 {
		return p
	}

	b := make([]byte, e.blockSize+4)

	return &b
}

// putBlock stores one block for owner and appends it to w; on failure it
// releases everything w holds.
func (e *Engine) putBlock(ctx context.Context, owner string, data []byte, w *written) error {
	h := block.Sum(data)

	if err := e.storeBlock(ctx, owner, h, data, nil); err != nil {
		e.release(ctx, owner, append(w.blocks, blockLoc{Hash: h}))

		return err
	}

	w.blocks = append(w.blocks, blockLoc{Hash: h, Size: int64(len(data))})

	return nil
}

func (e *Engine) PutObject(ctx context.Context, req *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
	enc, err := e.beginEncryption(req.ServerSideEncryption)
	if err != nil {
		return nil, err
	}

	c, err := e.cipher(enc, 0)
	if err != nil {
		return nil, err
	}

	cks, err := newChecksum(req.ChecksumAlgorithm)
	if err != nil {
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

	w, err := e.write(ctx, req.Reader, id, c, cks)
	if err != nil {
		return nil, err
	}

	if req.ContentMD5 != "" && req.ContentMD5 != w.etag {
		e.release(ctx, id, w.blocks)

		return nil, fs.ErrBadDigest
	}

	// Checked before the version exists, so a body that is not what the
	// client says it is never becomes one.
	if err := cks.verify(req.Checksum); err != nil {
		e.release(ctx, id, w.blocks)

		return nil, err
	}

	p := payload{
		Size:   w.size,
		ETag:   w.etag,
		Meta:   req.Metadata,
		Owner:  req.Owner,
		Inline: w.inline,
		Blocks: w.blocks,
		Enc:    enc,

		ChecksumAlgorithm: string(cks.algorithm),
		Checksum:          cks.value(),
	}

	if p.Checksum != "" {
		// A single PUT is one whole object: its digest is of the body itself.
		p.ChecksumType = string(checksum.FullObject)
	}

	if err := e.commit(ctx, inc.ID, req.Key, id, !versioned, cond, p, attrs{Tags: req.Tags, ACL: req.ACL}); err != nil {
		e.release(ctx, id, w.blocks)

		return nil, err
	}

	resp := &fs.PutObjectResponse{
		ETag:                 w.etag,
		ServerSideEncryption: algorithm(enc),
		ChecksumAlgorithm:    p.ChecksumAlgorithm,
		Checksum:             p.Checksum,
	}
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
		ETag:              p.ETag,
		Size:              p.Size,
		LastModified:      p.LastModified,
		Parts:             p.Parts,
		UploadID:          p.UploadID,
		ChecksumAlgorithm: p.ChecksumAlgorithm,
		Checksum:          p.Checksum,
		ChecksumType:      p.ChecksumType,
	}, nil
}
