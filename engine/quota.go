package engine

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/cluster/table"
)

// Quotas, as Ceph RGW has them: a bucket may be limited in the bytes and
// objects it holds, and so may an account — every bucket one owner owns,
// together. A write that would take either past its limit is refused with
// fs.ErrQuotaExceeded; deletes, and writes that do not grow what is stored,
// never are.
//
// Usage is tallied by every replica of a bucket's objects as rows change
// (table.CountUsage) and read at quorum, so it is exact once replicas have
// converged. Writes check it through a cache that lives for
// Config.QuotaCacheTTL and that this node's own writes keep current; other
// nodes' writes show up when it expires. Enforcement is therefore soft, as
// in RGW: concurrent writers through several nodes can together overshoot a
// limit by what they write within one TTL. A usage that cannot be read
// enforces nothing rather than refusing the write — the bucket's own write
// quorum is what decides whether it can be stored.

// settingQuota is the bucket setting holding its Quota.
const settingQuota = "quota"

// DefaultQuotaCacheTTL is how long a node trusts usage it read.
const DefaultQuotaCacheTTL = 10 * time.Second

// Quota limits what a bucket, or an account's buckets together, hold.
type Quota struct {
	// Enabled turns the quota on; a disabled one keeps its limits.
	Enabled bool `json:"enabled"`
	// MaxSize is the limit on bytes stored; negative for none.
	MaxSize int64 `json:"max_size"`
	// MaxObjects is the limit on objects stored; negative for none.
	MaxObjects int64 `json:"max_objects"`
}

// Unlimited is a quota that limits nothing, and the quota of a bucket or
// account that never had one set.
var Unlimited = Quota{MaxSize: -1, MaxObjects: -1}

// limits reports whether q can refuse anything.
func (q Quota) limits() bool { return q.Enabled && (q.MaxSize >= 0 || q.MaxObjects >= 0) }

// exceeded reports whether adding d to u breaks q. Only what d grows is
// checked: a write that frees space is never refused, even past the limit.
func (q Quota) exceeded(u, d Usage) bool {
	if !q.Enabled {
		return false
	}

	if q.MaxObjects >= 0 && d.Objects > 0 && u.Objects+d.Objects > q.MaxObjects {
		return true
	}

	return q.MaxSize >= 0 && d.Bytes > 0 && u.Bytes+d.Bytes > q.MaxSize
}

// Usage is what a bucket or an account stores: every object version that
// holds content — current or not, delete markers and uploads in flight
// excluded — and their bytes, as written by the client.
type Usage struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

func (u Usage) add(o Usage) Usage {
	return Usage{Objects: u.Objects + o.Objects, Bytes: u.Bytes + o.Bytes}
}

func usageFrom(u table.Usage) Usage { return Usage{Objects: u.Items, Bytes: u.Bytes} }

// objectUsage is what a key's row stores, the objects table's tally.
func objectUsage(o meta.Object) table.Usage {
	var u table.Usage

	for _, v := range o.Versions {
		if v.State != meta.Complete || v.DeleteMarker {
			continue
		}

		var p struct {
			Size int64 `json:"size"`
		}

		_ = json.Unmarshal(v.Payload, &p)

		u.Items++
		u.Bytes += p.Size
	}

	return u
}

// growth is what writing next over before changes in what the key stores.
func growth(before, next meta.Object) Usage {
	return usageFrom(objectUsage(meta.MergeObject(before, next)).Sub(objectUsage(before)))
}

// QuotaStats count quota enforcement since start.
type QuotaStats struct {
	// RejectedBucket and RejectedAccount count writes refused by a bucket's
	// quota and by its owner's.
	RejectedBucket  int64
	RejectedAccount int64
	// Checks counts writes checked against a quota, and LookupFailures the
	// usage reads that failed — writes let through unchecked unless an
	// older reading was at hand.
	Checks         int64
	LookupFailures int64
}

// quotaCache holds usage and account quotas read recently.
type quotaCache struct {
	ttl time.Duration

	mu       sync.Mutex
	buckets  map[string]cached[Usage]    // Usage by bucket ID.
	accounts map[string]cached[Quota]    // Quota by owner ID.
	owned    map[string]cached[[]string] // Bucket IDs by owner ID.
	stats    QuotaStats
}

type cached[T any] struct {
	v  T
	at time.Time
}

func newQuotaCache(ttl time.Duration) *quotaCache {
	return &quotaCache{
		ttl:      ttl,
		buckets:  map[string]cached[Usage]{},
		accounts: map[string]cached[Quota]{},
		owned:    map[string]cached[[]string]{},
	}
}

// load returns m[k] if it is fresh, else what fn reads, kept in m. When fn
// fails, an expired value is still better than none; ok is false only when
// there is nothing at all.
func load[T any](c *quotaCache, m map[string]cached[T], k string, now time.Time, fn func() (T, error)) (v T, ok bool) {
	c.mu.Lock()
	got, hit := m[k]
	c.mu.Unlock()

	if hit && now.Sub(got.at) < c.ttl {
		return got.v, true
	}

	fresh, err := fn()

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		c.stats.LookupFailures++

		return got.v, hit
	}

	m[k] = cached[T]{v: fresh, at: now}

	return fresh, true
}

// charge adds d to a bucket's cached usage, so this node's own writes count
// at once.
func (c *quotaCache) charge(bucketID string, d Usage) {
	if d == (Usage{}) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if got, ok := c.buckets[bucketID]; ok {
		got.v = got.v.add(d)
		c.buckets[bucketID] = got
	}
}

// forgetAccount drops what is cached about an account, after it changed.
func (c *quotaCache) forgetAccount(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.accounts, owner)
	delete(c.owned, owner)
}

// charge records what a write that landed changed in a bucket's usage.
func (e *Engine) charge(bucketID string, before, written meta.Object) {
	e.quota.charge(bucketID, growth(before, written))
}

// limit is one quota a write must fit, with the usage it is held against.
type limit struct {
	account string // Empty for the bucket's own quota.
	quota   Quota
	usage   func() (Usage, bool)
}

// limits are the quotas a write to b must fit: the bucket's and its
// owner's, those enabled.
func (e *Engine) limits(ctx context.Context, b meta.Bucket, inc meta.Incarnation) []limit {
	var out []limit

	if q := bucketQuota(b); q.limits() {
		out = append(out, limit{quota: q, usage: func() (Usage, bool) { return e.cachedBucketUsage(ctx, inc.ID) }})
	}

	owner := parseOwner(inc.Owner).ID
	if owner == "" {
		return out
	}

	q, ok := load(e.quota, e.quota.accounts, owner, e.now(), func() (Quota, error) {
		return e.accountQuota(ctx, owner)
	})
	if ok && q.limits() {
		out = append(out, limit{account: owner, quota: q, usage: func() (Usage, bool) { return e.cachedAccountUsage(ctx, owner) }})
	}

	return out
}

// enforce refuses a write that grows usage by d past any of limits.
func (e *Engine) enforce(limits []limit, d Usage) error {
	if len(limits) == 0 || (d.Objects <= 0 && d.Bytes <= 0) {
		return nil
	}

	e.quota.mu.Lock()
	e.quota.stats.Checks++
	e.quota.mu.Unlock()

	for _, l := range limits {
		u, ok := l.usage()
		if !ok || !l.quota.exceeded(u, d) {
			continue
		}

		e.quota.mu.Lock()
		if l.account == "" {
			e.quota.stats.RejectedBucket++
		} else {
			e.quota.stats.RejectedAccount++
		}
		e.quota.mu.Unlock()

		if l.account == "" {
			return errors.Wrapf(fs.ErrQuotaExceeded, "bucket quota: %s", describe(l.quota, u))
		}

		return errors.Wrapf(fs.ErrQuotaExceeded, "quota of account %q: %s", l.account, describe(l.quota, u))
	}

	return nil
}

func describe(q Quota, u Usage) string {
	var parts []string

	if q.MaxObjects >= 0 {
		parts = append(parts, fmtInt(u.Objects)+" of "+fmtInt(q.MaxObjects)+" objects")
	}

	if q.MaxSize >= 0 {
		parts = append(parts, fmtInt(u.Bytes)+" of "+fmtInt(q.MaxSize)+" bytes")
	}

	return strings.Join(parts, ", ")
}

func fmtInt(n int64) string { return strconv.FormatInt(n, 10) }

func (e *Engine) cachedBucketUsage(ctx context.Context, bucketID string) (Usage, bool) {
	return load(e.quota, e.quota.buckets, bucketID, e.now(), func() (Usage, error) {
		u, err := e.objects.Usage(ctx, bucketID)

		return usageFrom(u), err
	})
}

func (e *Engine) cachedAccountUsage(ctx context.Context, owner string) (Usage, bool) {
	ids, ok := load(e.quota, e.quota.owned, owner, e.now(), func() ([]string, error) {
		return e.ownedBuckets(ctx, owner)
	})
	if !ok {
		return Usage{}, false
	}

	var total Usage

	for _, id := range ids {
		u, ok := e.cachedBucketUsage(ctx, id)
		if !ok {
			return Usage{}, false
		}

		total = total.add(u)
	}

	return total, true
}

// ownedBuckets returns the IDs of the live buckets owner owns.
func (e *Engine) ownedBuckets(ctx context.Context, owner string) ([]string, error) {
	var (
		ids   []string
		start string
	)

	for {
		page, err := e.buckets.Range(ctx, bucketsPK, start, 1000)
		if err != nil {
			return nil, err
		}

		for _, r := range page {
			if inc, ok := r.Row.Live(); ok && parseOwner(inc.Owner).ID == owner {
				ids = append(ids, inc.ID)
			}
		}

		if len(page) < 1000 {
			return ids, nil
		}

		start = page[len(page)-1].SK + "\x00"
	}
}

func bucketQuota(b meta.Bucket) Quota {
	q := Unlimited
	if s, ok := b.Settings[settingQuota]; ok {
		_ = json.Unmarshal(s.V, &q)
	}

	return q
}

func (q Quota) validate() error {
	if q.MaxSize < -1 || q.MaxObjects < -1 {
		return errors.Wrap(fs.ErrUnsupportedOperation, "a limit is -1 for none, or at least 0")
	}

	return nil
}

// BucketQuota returns a bucket's quota and what the bucket stores.
func (e *Engine) BucketQuota(ctx context.Context, bucket string) (Quota, Usage, error) {
	b, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return Quota{}, Usage{}, err
	}

	u, err := e.objects.Usage(ctx, inc.ID)
	if err != nil {
		return Quota{}, Usage{}, err
	}

	return bucketQuota(b), usageFrom(u), nil
}

// SetBucketQuota sets a bucket's quota. It applies to writes from now on —
// on other nodes once their cached usage expires — and never removes what
// the bucket already stores.
func (e *Engine) SetBucketQuota(ctx context.Context, bucket string, q Quota) error {
	if err := q.validate(); err != nil {
		return err
	}

	defer e.lock(bucketsPK, bucket)()

	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return err
	}

	return e.buckets.Insert(ctx, bucketsPK, bucket, meta.Bucket{Settings: map[string]meta.LWW[json.RawMessage]{
		settingQuota: {TS: meta.NextTS(e.ts(), b.Settings[settingQuota].TS), V: mustJSON(q)},
	}})
}

// accountQuota reads an account's quota; Unlimited when none was set.
func (e *Engine) accountQuota(ctx context.Context, owner string) (Quota, error) {
	r, found, err := e.quotas.Get(ctx, quotasPK, owner)
	if err != nil {
		return Quota{}, err
	}

	q := Unlimited
	if found && len(r.V) > 0 { // A null register leaves q as it is.
		if err := json.Unmarshal(r.V, &q); err != nil {
			return Quota{}, errors.Wrapf(err, "decode quota of %q", owner)
		}
	}

	return q, nil
}

// AccountQuota returns the quota of an account — the owner ID buckets
// record, the canonical user ID of the keys that created them — and what
// its buckets store together.
func (e *Engine) AccountQuota(ctx context.Context, owner string) (Quota, Usage, error) {
	q, err := e.accountQuota(ctx, owner)
	if err != nil {
		return Quota{}, Usage{}, err
	}

	ids, err := e.ownedBuckets(ctx, owner)
	if err != nil {
		return Quota{}, Usage{}, err
	}

	var total Usage

	for _, id := range ids {
		u, err := e.objects.Usage(ctx, id)
		if err != nil {
			return Quota{}, Usage{}, err
		}

		total = total.add(usageFrom(u))
	}

	return q, total, nil
}

// SetAccountQuota sets an account's quota, which every bucket it owns
// counts against together. Other nodes see it once their cache expires.
func (e *Engine) SetAccountQuota(ctx context.Context, owner string, q Quota) error {
	if owner == "" {
		return errors.Wrap(fs.ErrUnsupportedOperation, "empty account")
	}

	if err := q.validate(); err != nil {
		return err
	}

	defer e.lock(quotasPK, owner)()

	prev, _, err := e.quotas.Get(ctx, quotasPK, owner)
	if err != nil {
		return err
	}

	if err := e.quotas.Insert(ctx, quotasPK, owner, meta.LWW[json.RawMessage]{
		TS: meta.NextTS(e.ts(), prev.TS), V: mustJSON(q),
	}); err != nil {
		return err
	}

	e.quota.forgetAccount(owner)

	return nil
}

// quotasPK is the account quotas table's one partition.
const quotasPK = ""

// QuotaStats returns the quota enforcement counters.
func (e *Engine) QuotaStats() QuotaStats {
	e.quota.mu.Lock()
	defer e.quota.mu.Unlock()

	return e.quota.stats
}
