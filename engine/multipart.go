package engine

import (
	"cmp"
	"context"
	"crypto/md5" //nolint:gosec // MD5 is required for S3 ETag compatibility.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
)

// An upload is a version of its key in state Uploading, with the upload ID as
// its version ID; its parts are rows of the parts table under that ID.
// Completing it makes the same version complete, so a retried completion
// finds it done.

// uploadPayload is an upload's payload while it is in flight.
type uploadPayload struct {
	Initiated time.Time         `json:"initiated"`
	Meta      fs.ObjectMetadata `json:"meta,omitzero"`
	Owner     fs.Owner          `json:"owner,omitzero"`
	// Enc seals every part as it arrives, so no part sits in the clear.
	Enc *encInfo `json:"enc,omitempty"`
	// ChecksumAlgorithm digests every part; ChecksumType is what the
	// completed object's digest means.
	ChecksumAlgorithm string `json:"cksum_alg,omitempty"`
	ChecksumType      string `json:"cksum_type,omitempty"`
}

// partRecord is one uploaded part.
type partRecord struct {
	ETag         string     `json:"etag"`
	Size         int64      `json:"size"`
	LastModified time.Time  `json:"mtime"`
	Checksum     string     `json:"cksum,omitempty"`
	ChecksumAlg  string     `json:"cksum_alg,omitempty"`
	Inline       []byte     `json:"inline,omitempty"`
	Blocks       []blockLoc `json:"blocks,omitempty"`
}

// partSK keeps parts in number order.
func partSK(n int) string { return fmt.Sprintf("%05d", n) }

// partOwner names a part's references apart from every other upload of the
// same part number, so replacing a part can release exactly the old one.
func partOwner(uploadID, partID string) string { return uploadID + "/" + partID }

func (e *Engine) CreateMultipartUpload(ctx context.Context, req *fs.CreateMultipartUploadRequest) (*fs.MultipartUpload, error) {
	enc, err := e.beginEncryption(ctx, req.ServerSideEncryption)
	if err != nil {
		return nil, err
	}

	cksAlg, cksType, err := uploadChecksum(req.ChecksumAlgorithm, req.ChecksumType)
	if err != nil {
		return nil, err
	}

	_, inc, err := e.bucket(ctx, req.Bucket)
	if err != nil {
		return nil, err
	}

	defer e.lock(inc.ID, req.Key)()

	before, err := e.object(ctx, inc.ID, req.Key)
	if err != nil {
		return nil, err
	}

	id := newID()
	ts := e.nextTS(before)
	initiated := e.now().UTC()

	v := meta.Version{
		ID:    id,
		TS:    ts,
		State: meta.Uploading,
		Payload: mustJSON(uploadPayload{
			Initiated: initiated, Meta: req.Metadata, Owner: req.Owner, Enc: enc,
			ChecksumAlgorithm: string(cksAlg), ChecksumType: string(cksType),
		}),
		Attrs: meta.LWW[json.RawMessage]{TS: ts, V: mustJSON(attrs{Tags: req.Tags, ACL: req.ACL})},
	}
	v = withKey(v, enc, ts)

	// The upload and its entry in the bucket's index of uploads, together.
	if err := e.writeRows(ctx,
		row(e.objects, inc.ID, req.Key, meta.Object{Versions: []meta.Version{v}}),
		row(e.uploads, inc.ID, uploadSK(req.Key, id), meta.LWW[json.RawMessage]{TS: ts, V: mustJSON(uploadEntry{
			Initiated: initiated, ChecksumAlgorithm: string(cksAlg), ChecksumType: string(cksType),
		})}),
	); err != nil {
		return nil, err
	}

	return &fs.MultipartUpload{
		UploadID: id, Bucket: req.Bucket, Key: req.Key, Initiated: initiated,
		ChecksumAlgorithm: string(cksAlg), ChecksumType: string(cksType),
	}, nil
}

// upload returns the in-flight upload id of bucket/key.
func (e *Engine) upload(ctx context.Context, bucket, key, id string) (string, meta.Version, error) {
	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return "", meta.Version{}, err
	}

	o, err := e.object(ctx, inc.ID, key)
	if err != nil {
		return "", meta.Version{}, err
	}

	v, ok := o.Find(id)
	if !ok || v.State != meta.Uploading {
		return inc.ID, v, fs.ErrUploadNotFound
	}

	return inc.ID, v, nil
}

func (e *Engine) UploadPart(ctx context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
	_, v, err := e.upload(ctx, req.Bucket, req.Key, req.UploadID)
	if err != nil {
		return nil, err
	}

	up, err := decodeUpload(v)
	if err != nil {
		return nil, err
	}

	c, err := e.cipher(ctx, up.Enc, req.PartNumber)
	if err != nil {
		return nil, err
	}

	// Parts of an upload with an algorithm are all digested with it, since
	// the object's checksum is composed from theirs.
	cks, err := newChecksum(cmp.Or(up.ChecksumAlgorithm, req.ChecksumAlgorithm))
	if err != nil {
		return nil, err
	}

	owner := partOwner(req.UploadID, newID())

	b, _, err := e.bucket(ctx, req.Bucket)
	if err != nil {
		return nil, err
	}

	w, err := e.write(ctx, req.Reader, owner, c, cks, bucketScheme(b), false)
	if err != nil {
		return nil, err
	}

	if err := cks.verify(req.Checksum); err != nil {
		e.release(ctx, owner, w.blocks)

		return nil, err
	}

	w.etag = customerETag(w.etag, up.Enc)

	rec := partRecord{
		ETag: w.etag, Size: w.size, LastModified: e.now().UTC(), Inline: w.inline, Blocks: w.blocks,
		Checksum: cks.value(), ChecksumAlg: string(cks.algorithm),
	}

	prev, _, err := e.parts.Get(ctx, req.UploadID, partSK(req.PartNumber))
	if err != nil {
		e.release(ctx, owner, w.blocks)

		return nil, err
	}

	// The upload completed or aborted while this part was being written.
	if meta.IsDone(prev) {
		e.release(ctx, owner, w.blocks)

		return nil, fs.ErrUploadNotFound
	}

	row := meta.LWW[json.RawMessage]{TS: meta.NextTS(e.ts(), prev.TS), V: mustJSON(storedPart{rec, owner})}

	if err := e.parts.Insert(ctx, req.UploadID, partSK(req.PartNumber), row); err != nil {
		e.release(ctx, owner, w.blocks)

		return nil, err
	}

	// The part this one replaces is no longer referenced by the upload.
	if old, ok := decodePart(prev); ok {
		e.release(ctx, old.Owner, old.Blocks)
	}

	return &fs.Part{
		PartNumber: req.PartNumber, ETag: w.etag, Size: w.size, LastModified: rec.LastModified,
		Checksum: rec.Checksum, ChecksumAlgorithm: rec.ChecksumAlg,
	}, nil
}

type storedPart struct {
	partRecord
	Owner string `json:"owner"`
}

func decodePart(r meta.LWW[json.RawMessage]) (storedPart, bool) {
	var p storedPart
	if meta.IsDone(r) || len(r.V) == 0 || json.Unmarshal(r.V, &p) != nil {
		return p, false
	}

	return p, true
}

// uploadParts returns an upload's parts by number.
func (e *Engine) uploadParts(ctx context.Context, uploadID string) (map[int]storedPart, error) {
	out := map[int]storedPart{}
	start := ""

	for {
		page, err := e.parts.Range(ctx, uploadID, start, 1000)
		if err != nil {
			return nil, err
		}

		for _, r := range page {
			n, err := strconv.Atoi(r.SK)
			if err != nil {
				continue
			}

			if p, ok := decodePart(r.Row); ok {
				out[n] = p
			}
		}

		if len(page) < 1000 {
			return out, nil
		}

		start = page[len(page)-1].SK + "\x00"
	}
}

func (e *Engine) ListParts(ctx context.Context, bucket, key, uploadID string) ([]fs.Part, error) {
	if _, _, err := e.upload(ctx, bucket, key, uploadID); err != nil {
		return nil, err
	}

	parts, err := e.uploadParts(ctx, uploadID)
	if err != nil {
		return nil, err
	}

	out := make([]fs.Part, 0, len(parts))
	for n, p := range parts {
		out = append(out, fs.Part{
			PartNumber: n, ETag: p.ETag, Size: p.Size, LastModified: p.LastModified,
			Checksum: p.Checksum, ChecksumAlgorithm: p.ChecksumAlg,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })

	return out, nil
}

// uploadEntry is what the uploads index holds of an upload in flight:
// enough to list it without reading its object row.
type uploadEntry struct {
	Initiated         time.Time `json:"initiated"`
	ChecksumAlgorithm string    `json:"cksum_alg,omitempty"`
	ChecksumType      string    `json:"cksum_type,omitempty"`
}

// uploadSK is an upload's sort key in the index: its object key, NUL, its
// ID. Keys may hold NUL but IDs never do, so the last NUL splits them.
func uploadSK(key, id string) string { return key + "\x00" + id }

// ListMultipartUploads lists the bucket's uploads in flight, from its index
// of uploads: the cost is the uploads, not the keys.
func (e *Engine) ListMultipartUploads(ctx context.Context, bucket string) ([]fs.MultipartUpload, error) {
	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return nil, err
	}

	out := []fs.MultipartUpload{}
	start := ""

	for {
		page, err := e.uploads.Range(ctx, inc.ID, start, 1000)
		if err != nil {
			return nil, err
		}

		for _, r := range page {
			if meta.IsDone(r.Row) {
				continue
			}

			i := strings.LastIndexByte(r.SK, 0)
			if i < 0 {
				continue
			}

			var u uploadEntry

			_ = json.Unmarshal(r.Row.V, &u)

			out = append(out, fs.MultipartUpload{
				UploadID: r.SK[i+1:], Bucket: bucket, Key: r.SK[:i], Initiated: u.Initiated,
				ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType,
			})
		}

		if len(page) < 1000 {
			break
		}

		start = page[len(page)-1].SK + "\x00"
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}

		return out[i].UploadID < out[j].UploadID
	})

	return out, nil
}

func (e *Engine) CompleteMultipartUpload(
	ctx context.Context, req *fs.CompleteMultipartUploadRequest,
) (*fs.CompleteMultipartUploadResponse, error) {
	b, inc, err := e.bucket(ctx, req.Bucket)
	if err != nil {
		return nil, err
	}

	defer e.lock(inc.ID, req.Key)()

	before, err := e.object(ctx, inc.ID, req.Key)
	if err != nil {
		return nil, err
	}

	v, ok := before.Find(req.UploadID)

	switch {
	case ok && v.Completed && v.State == meta.Complete:
		// A retried completion: the upload is already the object.
		p, err := decodePayload(v)
		if err != nil {
			return nil, err
		}

		return completed(req, p), nil
	case !ok || v.State != meta.Uploading:
		return nil, fs.ErrUploadNotFound
	}

	up, err := decodeUpload(v)
	if err != nil {
		return nil, err
	}

	stored, err := e.uploadParts(ctx, req.UploadID)
	if err != nil {
		return nil, err
	}

	requested := slices.Clone(req.Parts)
	sort.Slice(requested, func(i, j int) bool { return requested[i].PartNumber < requested[j].PartNumber })

	st, err := state(before)
	if err != nil {
		return nil, err
	}

	if err := req.Conditions.CheckWrite(st); err != nil {
		return nil, err
	}

	h := md5.New() //nolint:gosec // MD5 is required for S3 ETag compatibility.

	var (
		p       = payload{Meta: up.Meta, Owner: up.Owner, UploadID: req.UploadID, Enc: up.Enc}
		used    = map[string]bool{}
		digests []partDigest
	)

	for _, cp := range requested {
		part, ok := stored[cp.PartNumber]
		if !ok {
			continue
		}

		// A completion naming a part's checksum must name the one it was
		// uploaded with.
		if cp.Checksum != "" && part.Checksum != "" && cp.Checksum != part.Checksum {
			return nil, errors.Wrapf(fs.ErrInvalidPart, "part %d checksum does not match what was uploaded", cp.PartNumber)
		}

		digests = append(digests, partDigest{digest: part.Checksum, size: part.Size})

		sum, err := hex.DecodeString(part.ETag)
		if err != nil {
			return nil, errors.Wrapf(err, "part %d etag", cp.PartNumber)
		}

		_, _ = h.Write(sum)

		p.Size += part.Size
		p.Parts = append(p.Parts, fs.ObjectPart{PartNumber: cp.PartNumber, Size: part.Size, ETag: part.ETag, Checksum: part.Checksum})
		used[part.Owner] = true

		if part.Inline != nil {
			// A small part becomes a block of the object: a multipart
			// object is assembled from blocks.
			var w written
			if err := e.putBlock(ctx, req.UploadID, part.Inline, &w); err != nil {
				return nil, err
			}

			p.Blocks = append(p.Blocks, w.blocks...)

			continue
		}

		for _, b := range part.Blocks {
			// The object references its blocks itself, so releasing the
			// parts below cannot collect them.
			if err := e.refs.Insert(ctx, b.Hash.String(), req.UploadID, meta.BlockRef{}); err != nil {
				return nil, err
			}
		}

		p.Blocks = append(p.Blocks, part.Blocks...)
	}

	p.ETag = fmt.Sprintf("%x-%d", h.Sum(nil), len(requested))

	if p.Checksum, err = completionChecksum(up.ChecksumAlgorithm, up.ChecksumType, digests, req.Checksum); err != nil {
		return nil, err
	}

	if p.Checksum != "" {
		p.ChecksumAlgorithm, p.ChecksumType = up.ChecksumAlgorithm, up.ChecksumType
	}

	p.LastModified = e.now().UTC()

	if p.Size == 0 {
		p.Blocks = nil
	}

	done := v
	done.TS = e.nextTS(before)
	done.Null = versioning(b) != fs.VersioningEnabled
	done.State = meta.Complete
	done.Payload = mustJSON(p)

	completedRow := meta.Object{Versions: []meta.Version{done}}
	if err := e.writeRows(ctx,
		row(e.objects, inc.ID, req.Key, completedRow),
		row(e.uploads, inc.ID, uploadSK(req.Key, req.UploadID), meta.Done),
	); err != nil {
		return nil, err
	}

	e.releaseReplaced(ctx, before, meta.MergeObject(before, completedRow))

	for _, part := range stored {
		e.release(ctx, part.Owner, part.Blocks)
	}

	e.finishParts(ctx, req.UploadID, stored)

	return completed(req, p), nil
}

func completed(req *fs.CompleteMultipartUploadRequest, p payload) *fs.CompleteMultipartUploadResponse {
	return &fs.CompleteMultipartUploadResponse{
		Location:             "/" + req.Bucket + "/" + req.Key,
		Bucket:               req.Bucket,
		Key:                  req.Key,
		ETag:                 p.ETag,
		ServerSideEncryption: algorithm(p.Enc),
		ChecksumAlgorithm:    p.ChecksumAlgorithm,
		Checksum:             p.Checksum,
		ChecksumType:         p.ChecksumType,
	}
}

func (e *Engine) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	bucketID, v, err := e.upload(ctx, bucket, key, uploadID)
	if err != nil {
		return err
	}

	defer e.lock(bucketID, key)()

	v.State = meta.Gone
	if err := e.writeRows(ctx,
		row(e.objects, bucketID, key, meta.Object{Versions: []meta.Version{v}}),
		row(e.uploads, bucketID, uploadSK(key, uploadID), meta.Done),
	); err != nil {
		return err
	}

	parts, err := e.uploadParts(ctx, uploadID)
	if err != nil {
		return err
	}

	for _, p := range parts {
		e.release(ctx, p.Owner, p.Blocks)
	}

	e.finishParts(ctx, uploadID, parts)

	return nil
}
