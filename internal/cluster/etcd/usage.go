package etcd

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// usagePrefix is the key namespace holding per-bucket object accounting.
func (c Config) usagePrefix() string { return c.Prefix + "/usage/buckets/" }

// usageKey is one bucket's usage record. S3 bucket names are restricted to
// lowercase letters, digits, dots and hyphens, so the name goes into the key
// unescaped and a listing of the prefix is a listing of the buckets.
func (c Config) usageKey(bucket string) string { return c.usagePrefix() + bucket }

// usageElectionPrefix is the election namespace for the single cluster-wide
// recount runner.
func (c Config) usageElectionPrefix() string { return c.Prefix + "/usage/leader" }

// usageCASAttempts bounds the compare-and-set retries one delta makes before
// giving up. Contention is per bucket and each attempt is a single round trip,
// so the loop converges quickly; the bound is there so a pathological hot
// bucket cannot spin forever, and a dropped delta is corrected by the next
// recount.
const usageCASAttempts = 8

// BucketUsage is the durable object-accounting record for one bucket: how many
// objects it holds and how many bytes they occupy.
//
// It exists because the answer cannot be computed cheaply on demand. Objects
// are spread over every disk in the cluster and the only cluster-wide record of
// one is its sidecar, so counting means a scatter-gather over every disk —
// which is what a listing already does, and why an admin API that had to list
// before answering "how big is this bucket" would be unusable at the size where
// the question matters.
type BucketUsage struct {
	Bucket  string `json:"bucket"`
	Objects int64  `json:"objects"`
	Bytes   int64  `json:"bytes"`
	// Updated is when a delta last moved these counters.
	Updated time.Time `json:"updated"`
	// Counted is when a full recount last anchored them. The gap between the
	// two is how much drift the record could have accumulated: deltas are
	// applied after the fact and a crash between a write and its delta is lost
	// until the next recount.
	Counted time.Time `json:"counted,omitzero"`
}

// encodeBucketUsage marshals a usage record.
func encodeBucketUsage(rec BucketUsage) ([]byte, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, errors.Wrap(err, "marshal bucket usage")
	}

	return data, nil
}

// decodeBucketUsage parses a stored usage record.
func decodeBucketUsage(data []byte) (BucketUsage, error) {
	var rec BucketUsage
	if err := json.Unmarshal(data, &rec); err != nil {
		return BucketUsage{}, errors.Wrap(err, "unmarshal bucket usage")
	}

	return rec, nil
}

// AddBucketUsage folds a delta into a bucket's counters, creating the record if
// this is the bucket's first accounted write.
//
// The read-modify-write runs as a compare-and-set on the record's revision, so
// two nodes accounting for concurrent writes cannot lose each other's delta —
// which a blind put would, silently and permanently.
func AddBucketUsage(ctx context.Context, client *clientv3.Client, cfg Config, bucket string, objects, bytes int64) error {
	cfg = cfg.withDefaults()

	key := cfg.usageKey(bucket)

	for range usageCASAttempts {
		resp, err := client.Get(ctx, key)
		if err != nil {
			return errors.Wrap(err, "read bucket usage")
		}

		rec := BucketUsage{Bucket: bucket}
		rev := int64(0)

		if len(resp.Kvs) > 0 {
			if rec, err = decodeBucketUsage(resp.Kvs[0].Value); err != nil {
				// A corrupt record is rebuilt from the delta rather than
				// blocking every write to the bucket; the recount fixes it.
				rec = BucketUsage{Bucket: bucket}
			}

			rec.Bucket = bucket
			rev = resp.Kvs[0].ModRevision
		}

		rec.Objects += objects
		rec.Bytes += bytes
		rec.Updated = time.Now().UTC()

		clamp(&rec)

		data, err := encodeBucketUsage(rec)
		if err != nil {
			return err
		}

		txn, err := client.Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).
			Then(clientv3.OpPut(key, string(data))).
			Commit()
		if err != nil {
			return errors.Wrap(err, "apply bucket usage delta")
		}

		if txn.Succeeded {
			return nil
		}
	}

	return errors.Errorf("bucket usage for %q lost %d compare-and-set races", bucket, usageCASAttempts)
}

// SetBucketUsage installs a recount result, carrying forward whatever was
// accounted while the recount ran.
//
// base is what the record read when the recount started: anything the counters
// gained since then belongs to writes the walk may not have seen, so it is
// re-applied on top of the authoritative total rather than discarded. Without
// that, every write concurrent with a long recount would be silently dropped —
// a bias that only ever undercounts, which is the direction that hides data.
func SetBucketUsage(ctx context.Context, client *clientv3.Client, cfg Config, bucket string, objects, bytes int64, base BucketUsage) error {
	cfg = cfg.withDefaults()

	key := cfg.usageKey(bucket)
	now := time.Now().UTC()

	for range usageCASAttempts {
		resp, err := client.Get(ctx, key)
		if err != nil {
			return errors.Wrap(err, "read bucket usage")
		}

		var (
			cur = BucketUsage{Bucket: bucket}
			rev = int64(0)
		)

		if len(resp.Kvs) > 0 {
			if cur, err = decodeBucketUsage(resp.Kvs[0].Value); err != nil {
				cur = BucketUsage{Bucket: bucket}
			}

			rev = resp.Kvs[0].ModRevision
		}

		rec := BucketUsage{
			Bucket:  bucket,
			Objects: objects + (cur.Objects - base.Objects),
			Bytes:   bytes + (cur.Bytes - base.Bytes),
			Updated: now,
			Counted: now,
		}

		clamp(&rec)

		data, err := encodeBucketUsage(rec)
		if err != nil {
			return err
		}

		txn, err := client.Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).
			Then(clientv3.OpPut(key, string(data))).
			Commit()
		if err != nil {
			return errors.Wrap(err, "store bucket usage")
		}

		if txn.Succeeded {
			return nil
		}
	}

	return errors.Errorf("bucket usage recount for %q lost %d compare-and-set races", bucket, usageCASAttempts)
}

// clamp keeps a record non-negative. A negative count is arithmetically
// possible — a delete whose create was never accounted for, after a crash —
// and it is never a truthful answer, so it reads as zero until the recount
// replaces it.
func clamp(rec *BucketUsage) {
	rec.Objects = max(rec.Objects, 0)
	rec.Bytes = max(rec.Bytes, 0)
}

// DeleteBucketUsage removes a bucket's record, for a bucket that no longer
// exists.
func DeleteBucketUsage(ctx context.Context, client *clientv3.Client, cfg Config, bucket string) error {
	cfg = cfg.withDefaults()

	if _, err := client.Delete(ctx, cfg.usageKey(bucket)); err != nil {
		return errors.Wrap(err, "delete bucket usage")
	}

	return nil
}

// LoadBucketUsage reads one bucket's record; present is false when the bucket
// has never been accounted for.
func LoadBucketUsage(ctx context.Context, client *clientv3.Client, cfg Config, bucket string) (rec BucketUsage, present bool, err error) {
	cfg = cfg.withDefaults()

	resp, err := client.Get(ctx, cfg.usageKey(bucket))
	if err != nil {
		return BucketUsage{}, false, errors.Wrap(err, "read bucket usage")
	}

	if len(resp.Kvs) == 0 {
		return BucketUsage{}, false, nil
	}

	rec, err = decodeBucketUsage(resp.Kvs[0].Value)
	if err != nil {
		return BucketUsage{}, false, err
	}

	rec.Bucket = bucket

	return rec, true, nil
}

// ListBucketUsage returns every bucket's record, sorted by bucket name. A
// record that fails to decode is skipped rather than failing the listing: one
// unreadable bucket must not hide the usage of every other.
func ListBucketUsage(ctx context.Context, client *clientv3.Client, cfg Config) ([]BucketUsage, error) {
	cfg = cfg.withDefaults()

	resp, err := client.Get(ctx, cfg.usagePrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, errors.Wrap(err, "list bucket usage")
	}

	out := make([]BucketUsage, 0, len(resp.Kvs))

	for _, kv := range resp.Kvs {
		rec, err := decodeBucketUsage(kv.Value)
		if err != nil {
			continue
		}

		rec.Bucket = string(kv.Key[len(cfg.usagePrefix()):])
		out = append(out, rec)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })

	return out, nil
}

// UsageLeadership is a held cluster-wide recount slot. The recount walks every
// disk in the cluster, so exactly one node runs it; the rest stand by and take
// over when the holder's lease expires.
type UsageLeadership struct {
	session  *concurrency.Session
	election *concurrency.Election
}

// CampaignUsage blocks until this candidate holds the recount leadership or ctx
// is done. It is a separate election from the rebalance one: a cluster that is
// busy relocating fragments is exactly when usage matters, and the two jobs
// must not queue behind each other.
func CampaignUsage(ctx context.Context, client *clientv3.Client, cfg Config, candidate string) (*UsageLeadership, error) {
	cfg = cfg.withDefaults()

	session, err := concurrency.NewSession(client, concurrency.WithTTL(int(cfg.TTL)))
	if err != nil {
		return nil, errors.Wrap(err, "usage election session")
	}

	election := concurrency.NewElection(session, cfg.usageElectionPrefix())

	if err := election.Campaign(ctx, candidate); err != nil {
		_ = session.Close()
		return nil, errors.Wrap(err, "campaign usage leadership")
	}

	return &UsageLeadership{session: session, election: election}, nil
}

// Done is closed when leadership is lost involuntarily (the lease expired); the
// holder must stop recounting immediately.
func (l *UsageLeadership) Done() <-chan struct{} { return l.session.Done() }

// Close releases leadership so a standby can take over without waiting out the
// lease.
func (l *UsageLeadership) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	err := l.election.Resign(ctx)

	if cerr := l.session.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return errors.Wrap(err, "release usage leadership")
	}

	return nil
}
