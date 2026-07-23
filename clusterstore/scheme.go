package clusterstore

import (
	"context"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// schemeCacheTTL bounds how stale a remote bucket scheme change can look to
// this node's writes: within the TTL a write may still land at the previous
// scheme, and the next repair pass converts it. Short enough to converge
// quickly, long enough that hot buckets don't refetch their record per write.
const schemeCacheTTL = 5 * time.Second

// cachedScheme is one bucket's resolved scheme with its expiry.
type cachedScheme struct {
	s       scheme.Scheme
	expires time.Time
}

// EffectiveScheme resolves the scheme new writes to the bucket use: the
// bucket record's override when set, the configured default otherwise (also
// when no bucket record exists — direct coordinator writes). Results are
// cached for schemeCacheTTL.
//
// Resolution is lenient: when the record is momentarily unreachable the last
// cached value (expired or not) applies, then the configured default — a
// write is never refused over the bucket record, it lands durably at the
// fallback scheme and the next repair pass converts it. Conversion itself
// uses the strict resolveBucketScheme.
func (c *Coordinator) EffectiveScheme(ctx context.Context, bucket string) scheme.Scheme {
	s, err := c.freshBucketScheme(ctx, bucket)
	if err != nil {
		c.onErr(bucket, "", errors.Wrap(err, "resolve bucket scheme; writing at the last known"))

		if stale, ok := c.cachedBucketScheme(bucket); ok {
			return stale
		}

		return c.schemeFor(bucket)
	}

	return s
}

// freshBucketScheme is the strict, TTL-cached resolution: a fresh cache entry
// or a successful record read; an unreachable record is an error. Conversion
// uses it directly — it must skip rather than guess.
func (c *Coordinator) freshBucketScheme(ctx context.Context, bucket string) (scheme.Scheme, error) {
	c.schemeMu.Lock()
	if e, ok := c.schemeCache[bucket]; ok && time.Now().Before(e.expires) {
		c.schemeMu.Unlock()
		return e.s, nil
	}
	c.schemeMu.Unlock()

	s, err := c.resolveBucketScheme(ctx, bucket)
	if err != nil {
		return scheme.Scheme{}, err
	}

	c.cacheBucketScheme(bucket, s)

	return s, nil
}

// resolveBucketScheme reads the bucket's scheme from its record: the override
// when set, the configured default when absent or when no record exists. An
// unreachable record is an error — callers that must not guess (conversion)
// skip instead of falling back.
func (c *Coordinator) resolveBucketScheme(ctx context.Context, bucket string) (scheme.Scheme, error) {
	info, err := c.fetchBucket(ctx, c.topo.Topology(), bucket)
	if err != nil {
		if errors.Is(err, fs.ErrBucketNotFound) {
			return c.schemeFor(bucket), nil
		}

		return scheme.Scheme{}, errors.Wrapf(err, "fetch record of bucket %q", bucket)
	}

	return c.parseBucketScheme(info), nil
}

// parseBucketScheme resolves a bucket record's scheme override, falling back
// to the configured default when absent or unparseable (a corrupt override
// must not brick the bucket; SetBucketScheme always writes the normalized
// form).
func (c *Coordinator) parseBucketScheme(info *BucketInfo) scheme.Scheme {
	if info.Scheme == "" {
		return c.schemeFor(info.Name)
	}

	s, err := scheme.Parse(info.Scheme)
	if err != nil {
		c.onErr(info.Name, "", errors.Wrapf(err, "invalid bucket scheme %q, using default", info.Scheme))

		return c.schemeFor(info.Name)
	}

	return s
}

// cacheBucketScheme stores a bucket's resolved scheme for schemeCacheTTL.
func (c *Coordinator) cacheBucketScheme(bucket string, s scheme.Scheme) {
	c.schemeMu.Lock()
	defer c.schemeMu.Unlock()

	if c.schemeCache == nil {
		c.schemeCache = make(map[string]cachedScheme)
	}

	c.schemeCache[bucket] = cachedScheme{s: s, expires: time.Now().Add(schemeCacheTTL)}
}

// cachedBucketScheme returns the bucket's last resolved scheme, expired or
// not: read-path candidate sizing and outage fallback want the best known
// value without a fetch.
func (c *Coordinator) cachedBucketScheme(bucket string) (scheme.Scheme, bool) {
	c.schemeMu.Lock()
	defer c.schemeMu.Unlock()

	e, ok := c.schemeCache[bucket]

	return e.s, ok
}
