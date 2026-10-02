package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
)

// bucketsPK is the buckets table's one partition: every bucket name sorts in
// it, so listing buckets is one scan.
const bucketsPK = ""

// Bucket setting names.
const (
	settingACL       = "acl"
	settingCORS      = "cors"
	settingLifecycle = "lifecycle"
	settingPAB       = "public_access_block"
	settingOwnership = "object_ownership"
)

// bucket returns the live bucket and its incarnation, or ErrBucketNotFound.
func (e *Engine) bucket(ctx context.Context, name string) (meta.Bucket, meta.Incarnation, error) {
	b, _, err := e.buckets.Get(ctx, bucketsPK, name)
	if err != nil {
		return b, meta.Incarnation{}, err
	}

	inc, ok := b.Live()
	if !ok {
		return b, inc, fs.ErrBucketNotFound
	}

	return b, inc, nil
}

func (e *Engine) ListBuckets(ctx context.Context) ([]fs.Bucket, error) {
	var (
		out   []fs.Bucket
		start string
	)

	for {
		page, err := e.buckets.Range(ctx, bucketsPK, start, 1000)
		if err != nil {
			return nil, err
		}

		for _, r := range page {
			if _, ok := r.Row.Live(); ok {
				out = append(out, fs.Bucket{Name: r.SK, CreationDate: time.Unix(0, r.Row.Incarnation.TS)})
			}
		}

		if len(page) < 1000 {
			return out, nil
		}

		start = page[len(page)-1].SK + "\x00"
	}
}

func (e *Engine) CreateBucket(ctx context.Context, name string) error {
	return e.CreateBucketOwned(ctx, name, fs.Owner{})
}

// CreateBucketOwned implements fs.BucketOwnership. The new incarnation gets a
// fresh ID, so a name deleted and created again starts empty.
func (e *Engine) CreateBucketOwned(ctx context.Context, name string, owner fs.Owner) error {
	defer e.lock(bucketsPK, name)()

	b, _, err := e.buckets.Get(ctx, bucketsPK, name)
	if err != nil {
		return err
	}

	if _, ok := b.Live(); ok {
		return errors.Wrapf(fs.ErrBucketAlreadyExists, "bucket %q", name)
	}

	return e.buckets.Insert(ctx, bucketsPK, name, meta.Bucket{Incarnation: meta.LWW[meta.Incarnation]{
		TS: meta.NextTS(e.ts(), b.Incarnation.TS),
		V:  meta.Incarnation{ID: newID(), Owner: ownerString(owner)},
	}})
}

// BucketOwner implements fs.BucketOwnership.
func (e *Engine) BucketOwner(ctx context.Context, name string) (fs.Owner, error) {
	_, inc, err := e.bucket(ctx, name)
	if err != nil {
		return fs.Owner{}, err
	}

	return parseOwner(inc.Owner), nil
}

func (e *Engine) BucketExists(ctx context.Context, name string) (bool, error) {
	_, _, err := e.bucket(ctx, name)
	if errors.Is(err, fs.ErrBucketNotFound) {
		return false, nil
	}

	return err == nil, err
}

// DeleteBucket deletes an empty bucket. Emptiness is checked on the listing a
// quorum returns, not under a lock: see the package doc.
func (e *Engine) DeleteBucket(ctx context.Context, name string) error {
	defer e.lock(bucketsPK, name)()

	b, inc, err := e.bucket(ctx, name)
	if err != nil {
		return err
	}

	empty, err := e.bucketEmpty(ctx, inc.ID)
	if err != nil {
		return err
	}

	if !empty {
		return fs.ErrBucketNotEmpty
	}

	inc.Deleted = true

	return e.buckets.Insert(ctx, bucketsPK, name, meta.Bucket{Incarnation: meta.LWW[meta.Incarnation]{
		TS: meta.NextTS(e.ts(), b.Incarnation.TS),
		V:  inc,
	}})
}

func (e *Engine) bucketEmpty(ctx context.Context, bucketID string) (bool, error) {
	start := ""

	for {
		page, err := e.objects.Range(ctx, bucketID, start, 1000)
		if err != nil {
			return false, err
		}

		for _, r := range page {
			if _, ok := r.Row.Current(); ok {
				return false, nil
			}
		}

		if len(page) < 1000 {
			return true, nil
		}

		start = page[len(page)-1].SK + "\x00"
	}
}

// setting reads one bucket setting into v; ok is false when it is unset.
func (e *Engine) setting(ctx context.Context, bucket, name string, v any) (bool, error) {
	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return false, err
	}

	s, ok := b.Settings[name]
	if !ok || len(s.V) == 0 || string(s.V) == "null" {
		return false, nil
	}

	return true, json.Unmarshal(s.V, v)
}

// setSetting writes one bucket setting; nil clears it.
func (e *Engine) setSetting(ctx context.Context, bucket, name string, v any) error {
	defer e.lock(bucketsPK, bucket)()

	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return err
	}

	return e.buckets.Insert(ctx, bucketsPK, bucket, meta.Bucket{Settings: map[string]meta.LWW[json.RawMessage]{
		name: {TS: meta.NextTS(e.ts(), b.Settings[name].TS), V: mustJSON(v)},
	}})
}

func (e *Engine) SetBucketACL(ctx context.Context, bucket string, acl fs.ACL) error {
	return e.setSetting(ctx, bucket, settingACL, acl)
}

func (e *Engine) BucketACL(ctx context.Context, bucket string) (fs.ACL, error) {
	var acl fs.ACL
	if _, err := e.setting(ctx, bucket, settingACL, &acl); err != nil {
		return fs.ACLPrivate, err
	}

	return normalizeACL(acl), nil
}

// BucketCORS implements fs.BucketCORSStore.
func (e *Engine) BucketCORS(ctx context.Context, bucket string) ([]fs.CORSRule, error) {
	var rules []fs.CORSRule

	_, err := e.setting(ctx, bucket, settingCORS, &rules)

	return rules, err
}

// SetBucketCORS implements fs.BucketCORSStore.
func (e *Engine) SetBucketCORS(ctx context.Context, bucket string, rules []fs.CORSRule) error {
	return e.setSetting(ctx, bucket, settingCORS, rules)
}

// DeleteBucketCORS implements fs.BucketCORSStore.
func (e *Engine) DeleteBucketCORS(ctx context.Context, bucket string) error {
	return e.setSetting(ctx, bucket, settingCORS, nil)
}

// BucketLifecycle implements fs.BucketLifecycleStore.
func (e *Engine) BucketLifecycle(ctx context.Context, bucket string) ([]fs.LifecycleRule, error) {
	var rules []fs.LifecycleRule

	_, err := e.setting(ctx, bucket, settingLifecycle, &rules)

	return rules, err
}

// SetBucketLifecycle implements fs.BucketLifecycleStore.
func (e *Engine) SetBucketLifecycle(ctx context.Context, bucket string, rules []fs.LifecycleRule) error {
	return e.setSetting(ctx, bucket, settingLifecycle, rules)
}

// DeleteBucketLifecycle implements fs.BucketLifecycleStore.
func (e *Engine) DeleteBucketLifecycle(ctx context.Context, bucket string) error {
	return e.setSetting(ctx, bucket, settingLifecycle, nil)
}

// BucketPublicAccessBlock implements fs.BucketSettingsStore.
func (e *Engine) BucketPublicAccessBlock(ctx context.Context, bucket string) (*fs.PublicAccessBlock, error) {
	var pab fs.PublicAccessBlock

	ok, err := e.setting(ctx, bucket, settingPAB, &pab)
	if err != nil || !ok {
		return nil, err
	}

	return &pab, nil
}

// SetBucketPublicAccessBlock implements fs.BucketSettingsStore.
func (e *Engine) SetBucketPublicAccessBlock(ctx context.Context, bucket string, pab *fs.PublicAccessBlock) error {
	return e.setSetting(ctx, bucket, settingPAB, pab)
}

// BucketObjectOwnership implements fs.BucketSettingsStore.
func (e *Engine) BucketObjectOwnership(ctx context.Context, bucket string) (string, error) {
	var ownership string

	_, err := e.setting(ctx, bucket, settingOwnership, &ownership)

	return ownership, err
}

// SetBucketObjectOwnership implements fs.BucketSettingsStore.
func (e *Engine) SetBucketObjectOwnership(ctx context.Context, bucket, ownership string) error {
	return e.setSetting(ctx, bucket, settingOwnership, ownership)
}

func normalizeACL(a fs.ACL) fs.ACL {
	if a == "" {
		return fs.ACLPrivate
	}

	return a
}

// ownerString and parseOwner keep an owner in the incarnation's one field.
func ownerString(o fs.Owner) string {
	if o.IsZero() {
		return ""
	}

	return string(mustJSON(o))
}

func parseOwner(s string) fs.Owner {
	var o fs.Owner

	_ = json.Unmarshal([]byte(s), &o)

	return o
}
