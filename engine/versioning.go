package engine

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/cluster/table"
)

// Versioning is a view over the version list every key already has. A write
// while versioning is enabled adds a version that nothing replaces; one while
// it is unset or suspended adds a null version, which replaces the key's older
// null versions in the merge. Uploads in flight and versions deleted
// permanently are never visible.

var (
	_ fs.Versioner                 = (*Engine)(nil)
	_ fs.ConditionalVersionDeleter = (*Engine)(nil)
)

const settingVersioning = "versioning"

// versioning returns a bucket's versioning state.
func versioning(b meta.Bucket) fs.VersioningState {
	var state fs.VersioningState
	if s, ok := b.Settings[settingVersioning]; ok {
		_ = json.Unmarshal(s.V, &state)
	}

	return state
}

// SetBucketVersioning implements fs.Versioner.
func (e *Engine) SetBucketVersioning(ctx context.Context, bucket string, state fs.VersioningState) error {
	if state == fs.VersioningUnset {
		return errors.Wrap(fs.ErrUnsupportedOperation, "a bucket cannot return to never-versioned")
	}

	return e.setSetting(ctx, bucket, settingVersioning, state)
}

// BucketVersioning implements fs.Versioner.
func (e *Engine) BucketVersioning(ctx context.Context, bucket string) (fs.VersioningState, error) {
	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return fs.VersioningUnset, err
	}

	return versioning(b), nil
}

// s3ID is the version ID S3 reports for v.
func s3ID(v meta.Version) string {
	if v.Null {
		return fs.NullVersionID
	}

	return v.ID
}

// visible reports whether a version is part of the key's history as S3 shows
// it: complete, including delete markers.
func visible(v meta.Version) bool { return v.State == meta.Complete }

// find returns the version an S3 version ID names: "null" is the newest null
// version.
func find(o meta.Object, id string) (meta.Version, bool) {
	for i := len(o.Versions) - 1; i >= 0; i-- {
		v := o.Versions[i]
		if !visible(v) {
			continue
		}

		if s3ID(v) == id {
			return v, true
		}
	}

	return meta.Version{}, false
}

// GetObjectVersion implements fs.Versioner.
func (e *Engine) GetObjectVersion(ctx context.Context, bucket, key, versionID string) (*fs.GetObjectResponse, error) {
	if !fs.ValidVersionID(versionID) {
		return nil, fs.ErrObjectNotFound
	}

	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return nil, err
	}

	o, err := e.object(ctx, inc.ID, key)
	if err != nil {
		return nil, err
	}

	v, ok := find(o, versionID)

	switch {
	case !ok:
		return nil, fs.ErrObjectNotFound
	case v.DeleteMarker:
		return nil, fs.ErrMethodNotAllowedOnDeleteMarker
	}

	return e.response(ctx, v)
}

// response serves version v.
func (e *Engine) response(ctx context.Context, v meta.Version) (*fs.GetObjectResponse, error) {
	p, err := decodePayload(v)
	if err != nil {
		return nil, err
	}

	r, err := e.reader(ctx, p)
	if err != nil {
		return nil, err
	}

	resp := &fs.GetObjectResponse{
		Reader:               r,
		ServerSideEncryption: algorithm(p.Enc),
		Size:                 p.Size,
		LastModified:         p.LastModified,
		ETag:                 p.ETag,
		Metadata:             p.Meta,
		TagCount:             len(decodeAttrs(v).Tags),
		ChecksumAlgorithm:    p.ChecksumAlgorithm,
		Checksum:             p.Checksum,
		ChecksumType:         p.ChecksumType,
	}

	if !v.Null {
		resp.VersionID = v.ID
	}

	return resp, nil
}

// ListObjectVersions implements fs.Versioner. It reads the bucket's rows in
// key order from the marker, stops at the end of the prefix, and returns as
// soon as the page is full, so a page costs what it returns.
func (e *Engine) ListObjectVersions(ctx context.Context, req *fs.ListObjectVersionsRequest) (*fs.ListObjectVersionsResponse, error) {
	_, inc, err := e.bucket(ctx, req.Bucket)
	if err != nil {
		return nil, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 1000
	}

	// One row past the limit tells whether the listing is truncated; a
	// small page reads only that much.
	page := min(1000, limit+1)

	out := &fs.ListObjectVersionsResponse{}
	full := func() bool { return len(out.Versions)+len(out.CommonPrefixes) >= limit }
	start := max(req.Prefix, req.KeyMarker)

	for {
		rows, err := e.objects.RangePrefix(ctx, inc.ID, start, req.Prefix, page)
		if err != nil {
			return nil, err
		}

		next, jumped := "", false

		for _, r := range rows {
			if !strings.HasPrefix(r.SK, req.Prefix) {
				return out, nil // Keys sort after the prefix: nothing more matches.
			}

			if r.SK < req.KeyMarker {
				continue
			}

			versions, err := listVersions(r.SK, r.Row)
			if err != nil {
				return nil, err
			}

			if len(versions) == 0 {
				continue
			}

			if req.Delimiter != "" {
				if i := strings.Index(r.SK[len(req.Prefix):], req.Delimiter); i >= 0 {
					entry := r.SK[:len(req.Prefix)+i+len(req.Delimiter)]
					next, jumped = prefixEnd(entry), true

					// A prefix at or before the marker was returned already.
					if entry > req.KeyMarker {
						if full() {
							out.IsTruncated = true

							return out, nil
						}

						out.CommonPrefixes = append(out.CommonPrefixes, entry)
						out.NextKeyMarker, out.NextVersionIDMarker = entry, ""
					}

					break
				}
			}

			if r.SK == req.KeyMarker {
				versions = afterMarker(versions, req.VersionIDMarker)
			}

			for _, v := range versions {
				if full() {
					out.IsTruncated = true

					return out, nil
				}

				out.Versions = append(out.Versions, v)
				out.NextKeyMarker, out.NextVersionIDMarker = v.Key, v.VersionID
			}
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

// afterMarker returns the versions of the marker's key that come after the
// marker version, newest first. They are found by the marker's position: a
// version ID's sort order says nothing about "null", which sorts after every
// other ID wherever it sits in the history. With no version marker the key
// was finished on an earlier page.
func afterMarker(versions []fs.ObjectVersion, marker string) []fs.ObjectVersion {
	if marker == "" {
		return nil
	}

	for i, v := range versions {
		if v.VersionID == marker {
			return versions[i+1:]
		}
	}

	// The marker version is gone since: resume after where its ID places it,
	// which holds for every ID but "null".
	var out []fs.ObjectVersion

	for _, v := range versions {
		if v.VersionID != fs.NullVersionID && v.VersionID > marker {
			out = append(out, v)
		}
	}

	return out
}

// listVersions reports a key's visible versions, newest first.
func listVersions(key string, o meta.Object) ([]fs.ObjectVersion, error) {
	var out []fs.ObjectVersion

	for i := len(o.Versions) - 1; i >= 0; i-- {
		v := o.Versions[i]
		if !visible(v) {
			continue
		}

		p, err := decodePayload(v)
		if err != nil {
			return nil, err
		}

		out = append(out, fs.ObjectVersion{
			Key:          key,
			VersionID:    s3ID(v),
			IsLatest:     len(out) == 0,
			DeleteMarker: v.DeleteMarker,
			Size:         p.Size,
			ETag:         p.ETag,
			LastModified: p.LastModified,
			Owner:        p.Owner,
		})
	}

	return out, nil
}

// DeleteObjectVersion implements fs.Versioner.
func (e *Engine) DeleteObjectVersion(ctx context.Context, bucket, key, versionID string) (fs.DeleteResult, error) {
	return e.DeleteObjectVersionIf(ctx, bucket, key, versionID, fs.Conditions{})
}

// DeleteObjectVersionIf implements fs.ConditionalVersionDeleter. The condition
// is checked and the delete written under the key's lock.
func (e *Engine) DeleteObjectVersionIf(
	ctx context.Context, bucket, key, versionID string, cond fs.Conditions,
) (fs.DeleteResult, error) {
	if versionID != "" && !fs.ValidVersionID(versionID) {
		return fs.DeleteResult{}, fs.ErrObjectNotFound
	}

	b, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return fs.DeleteResult{}, err
	}

	state := versioning(b)

	defer e.lock(inc.ID, key)()

	before, err := e.object(ctx, inc.ID, key)
	if err != nil {
		return fs.DeleteResult{}, err
	}

	target, err := deleteTarget(before, versionID, state)
	if err != nil {
		return fs.DeleteResult{}, err
	}

	if err := cond.CheckDelete(target); err != nil {
		return fs.DeleteResult{}, err
	}

	switch {
	case versionID != "":
		return e.removeVersion(ctx, inc.ID, key, before, versionID)
	case state == fs.VersioningUnset:
		if !target.Exists {
			return fs.DeleteResult{}, nil // Deleting what is not there succeeds.
		}

		cur, _ := before.Current()

		_, err := e.removeVersion(ctx, inc.ID, key, before, s3ID(cur))

		return fs.DeleteResult{}, err
	}

	marker := meta.Version{
		ID:           newID(),
		TS:           e.nextTS(before),
		Null:         state == fs.VersioningSuspended,
		State:        meta.Complete,
		DeleteMarker: true,
	}

	row := meta.Object{Versions: []meta.Version{marker}}

	// The marker and the release of what it replaces, in one commit.
	markerRow, err := e.objects.Row(inc.ID, key, row)
	if err != nil {
		return fs.DeleteResult{}, err
	}

	released, err := e.releaseRows(before, meta.MergeObject(before, row))
	if err != nil {
		return fs.DeleteResult{}, err
	}

	if err := table.Write(ctx, e.member, append([]table.Row{markerRow}, released...)...); err != nil {
		return fs.DeleteResult{}, err
	}

	return fs.DeleteResult{VersionID: s3ID(marker), DeleteMarker: true}, nil
}

// removeVersion permanently deletes the version an S3 version ID names, and
// releases its blocks. Deleting a version that is not there succeeds.
func (e *Engine) removeVersion(
	ctx context.Context, bucketID, key string, o meta.Object, versionID string,
) (fs.DeleteResult, error) {
	v, ok := find(o, versionID)
	if !ok {
		return fs.DeleteResult{VersionID: versionID}, nil
	}

	gone := v
	gone.State = meta.Gone
	gone.Completed = true

	// The tombstone and the release of the version's blocks, in one commit.
	row, err := e.objects.Row(bucketID, key, meta.Object{Versions: []meta.Version{gone}})
	if err != nil {
		return fs.DeleteResult{}, err
	}

	rows := []table.Row{row}

	if p, err := decodePayload(v); err == nil {
		released, err := e.releaseBlocks(v.ID, p.Blocks)
		if err != nil {
			return fs.DeleteResult{}, err
		}

		rows = append(rows, released...)
	}

	if err := table.Write(ctx, e.member, rows...); err != nil {
		return fs.DeleteResult{}, err
	}

	return fs.DeleteResult{VersionID: versionID, DeleteMarker: v.DeleteMarker}, nil
}

// deleteTarget is the state a conditional delete is checked against: the
// version versionID names, or the key's current one.
//
// A key whose newest version is a delete marker over real content exists and
// matches nothing — S3 answers If-Match: * with success and a specific ETag
// with 412. Over nothing but markers there is nothing to guard.
func deleteTarget(o meta.Object, versionID string, state fs.VersioningState) (fs.ObjectState, error) {
	if versionID != "" {
		v, ok := find(o, versionID)
		if !ok || v.DeleteMarker {
			return fs.ObjectState{}, nil
		}

		return versionState(v)
	}

	for i := len(o.Versions) - 1; i >= 0; i-- {
		v := o.Versions[i]
		if !visible(v) {
			continue
		}

		if !v.DeleteMarker {
			return versionState(v)
		}

		if state == fs.VersioningUnset {
			return fs.ObjectState{}, nil
		}

		for _, w := range o.Versions[:i] {
			if visible(w) && !w.DeleteMarker {
				return fs.ObjectState{Exists: true}, nil
			}
		}

		return fs.ObjectState{}, nil
	}

	return fs.ObjectState{}, nil
}

func versionState(v meta.Version) (fs.ObjectState, error) {
	p, err := decodePayload(v)
	if err != nil {
		return fs.ObjectState{}, err
	}

	return fs.ObjectState{Exists: true, ETag: p.ETag, Size: p.Size, LastModified: p.LastModified}, nil
}
