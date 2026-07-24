package etcd

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// authPrefix is the namespace holding all cluster-wide auth state: the
// per-credential keys under keys/ and the public-read bucket list. A single
// watch over it drives every node's live auth store.
func (c Config) authPrefix() string { return c.Prefix + "/auth/" }

// authKeysPrefix is the namespace holding one key per credential. The record
// value is a JSON authRecord; the etcd key suffix is the access key ID.
func (c Config) authKeysPrefix() string { return c.authPrefix() + "keys/" }

// authKey is one credential's etcd key.
func (c Config) authKey(accessKey string) string { return c.authKeysPrefix() + accessKey }

// publicReadKey holds the JSON array of anonymously-readable buckets. It is a
// sibling of keys/, not under it, so the credential listing never sees it.
func (c Config) publicReadKey() string { return c.authPrefix() + "public-read" }

// AuthGrant is the wire form of one grant: a bucket pattern and a permission
// name ("read"/"write"/"admin"). The permission is stored as its name, not a
// numeric enum, so the format is stable across code changes.
type AuthGrant struct {
	Pattern    string `json:"pattern"`
	Permission string `json:"permission"`
}

// AuthRecord is the stored form of one cluster credential. The secret is sealed
// (encrypted with a key derived from the cluster secret) before it reaches
// etcd; this package treats SecretSealed as opaque and never sees plaintext.
type AuthRecord struct {
	AccessKey    string      `json:"access_key"`
	SecretSealed string      `json:"secret_sealed"`
	Grants       []AuthGrant `json:"grants,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
}

// encodeAuthRecord marshals a credential for its etcd key.
func encodeAuthRecord(rec AuthRecord) ([]byte, error) {
	// The access key is a public identifier and the secret is already sealed;
	// no plaintext secret is ever marshaled here.
	data, err := json.Marshal(rec) //nolint:gosec // No plaintext secret in the record.
	if err != nil {
		return nil, errors.Wrap(err, "marshal auth record")
	}

	return data, nil
}

// decodeAuthRecord parses a credential value.
func decodeAuthRecord(data []byte) (AuthRecord, error) {
	var rec AuthRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return AuthRecord{}, errors.Wrap(err, "unmarshal auth record")
	}

	if rec.AccessKey == "" {
		return AuthRecord{}, errors.New("auth record without access key")
	}

	return rec, nil
}

// CreateAuthKey stores rec only if its access key is not already present. It
// returns created=false (without error) when the key already exists, so the
// caller can report a conflict; the compare-and-set makes concurrent creation
// on two nodes safe (exactly one wins).
func CreateAuthKey(ctx context.Context, client *clientv3.Client, cfg Config, rec AuthRecord) (created bool, err error) {
	cfg = cfg.withDefaults()

	data, err := encodeAuthRecord(rec)
	if err != nil {
		return false, err
	}

	key := cfg.authKey(rec.AccessKey)

	resp, err := client.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, string(data))).
		Commit()
	if err != nil {
		return false, errors.Wrap(err, "create auth key")
	}

	return resp.Succeeded, nil
}

// DeleteAuthKey removes a credential. It returns deleted=false (without error)
// when the access key does not exist.
func DeleteAuthKey(ctx context.Context, client *clientv3.Client, cfg Config, accessKey string) (deleted bool, err error) {
	cfg = cfg.withDefaults()

	key := cfg.authKey(accessKey)

	resp, err := client.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "!=", 0)).
		Then(clientv3.OpDelete(key)).
		Commit()
	if err != nil {
		return false, errors.Wrap(err, "delete auth key")
	}

	return resp.Succeeded, nil
}

// ListAuthKeys returns every stored credential, sorted by access key, and the
// etcd revision the listing reflects.
func ListAuthKeys(ctx context.Context, client *clientv3.Client, cfg Config) (records []AuthRecord, revision int64, err error) {
	cfg = cfg.withDefaults()

	resp, err := client.Get(ctx, cfg.authKeysPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, 0, errors.Wrap(err, "list auth keys")
	}

	records = make([]AuthRecord, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		rec, err := decodeAuthRecord(kv.Value)
		if err != nil {
			// A malformed record must not deny every credential; skip it.
			continue
		}

		records = append(records, rec)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].AccessKey < records[j].AccessKey })

	return records, resp.Header.Revision, nil
}

// LoadPublicRead reads the cluster-wide public-read bucket list; present is
// false when no list has been stored yet (distinct from a stored empty list).
func LoadPublicRead(ctx context.Context, client *clientv3.Client, cfg Config) (buckets []string, present bool, err error) {
	cfg = cfg.withDefaults()

	resp, err := client.Get(ctx, cfg.publicReadKey())
	if err != nil {
		return nil, false, errors.Wrap(err, "load public-read buckets")
	}

	if len(resp.Kvs) == 0 {
		return nil, false, nil
	}

	buckets, err = decodePublicRead(resp.Kvs[0].Value)
	if err != nil {
		return nil, false, err
	}

	return buckets, true, nil
}

// SetPublicRead replaces the cluster-wide public-read bucket list. The change
// propagates to every node's live auth store through the watch.
func SetPublicRead(ctx context.Context, client *clientv3.Client, cfg Config, buckets []string) error {
	cfg = cfg.withDefaults()

	data, err := encodePublicRead(buckets)
	if err != nil {
		return err
	}

	if _, err := client.Put(ctx, cfg.publicReadKey(), string(data)); err != nil {
		return errors.Wrap(err, "store public-read buckets")
	}

	return nil
}

// SeedPublicRead stores buckets only if no list exists yet (compare-and-set on
// absence), so config seeds a fresh cluster once without clobbering a list an
// operator later set — including one deliberately emptied.
func SeedPublicRead(ctx context.Context, client *clientv3.Client, cfg Config, buckets []string) (seeded bool, err error) {
	cfg = cfg.withDefaults()

	data, err := encodePublicRead(buckets)
	if err != nil {
		return false, err
	}

	key := cfg.publicReadKey()

	resp, err := client.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, string(data))).
		Commit()
	if err != nil {
		return false, errors.Wrap(err, "seed public-read buckets")
	}

	return resp.Succeeded, nil
}

// encodePublicRead marshals the bucket list, normalizing nil to an empty array.
func encodePublicRead(buckets []string) ([]byte, error) {
	if buckets == nil {
		buckets = []string{}
	}

	data, err := json.Marshal(buckets)
	if err != nil {
		return nil, errors.Wrap(err, "marshal public-read buckets")
	}

	return data, nil
}

// decodePublicRead parses a stored public-read list.
func decodePublicRead(data []byte) ([]string, error) {
	var buckets []string
	if err := json.Unmarshal(data, &buckets); err != nil {
		return nil, errors.Wrap(err, "unmarshal public-read buckets")
	}

	return buckets, nil
}

// AuthSnapshot is the full cluster-wide auth state: every credential and the
// public-read bucket list, as of one watch update.
type AuthSnapshot struct {
	Records    []AuthRecord
	PublicRead []string
}

// AuthSource watches the whole cluster-wide auth namespace (credentials plus the
// public-read bucket list) and calls OnChange with the full snapshot whenever it
// changes (and once on start). It mirrors Source: a resilient, self-resyncing
// watch that keeps serving the last good snapshot across transient etcd
// failures. It is the node-side half of cluster-wide runtime key management —
// every node runs one and applies its output to the live auth store, so a
// credential or public-read change made anywhere propagates to all nodes.
type AuthSource struct {
	client *clientv3.Client
	cfg    Config

	onChange func(AuthSnapshot)

	records    map[string]AuthRecord
	publicRead []string
	cur        atomic.Pointer[AuthSnapshot]
	cancel     context.CancelFunc
	done       sync.WaitGroup

	// OnError observes background watch failures (the source keeps serving the
	// last snapshot and retries). Set before NewAuthSource; may be nil.
	OnError func(err error)
}

// NewAuthSource loads the current auth state, fires onChange once with it, and
// starts watching. onChange must not be nil and is invoked synchronously from
// the watch goroutine, so it should be cheap and non-blocking (applying an
// atomic auth-store snapshot qualifies).
func NewAuthSource(ctx context.Context, client *clientv3.Client, cfg Config, onChange func(AuthSnapshot)) (*AuthSource, error) {
	cfg = cfg.withDefaults()

	s := &AuthSource{
		client:   client,
		cfg:      cfg,
		onChange: onChange,
		records:  make(map[string]AuthRecord),
	}

	rev, err := s.load(ctx)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.done.Go(func() { s.watch(runCtx, rev+1) })

	return s, nil
}

// Snapshot returns the latest auth state (a lock-free read of the last
// published snapshot).
func (s *AuthSource) Snapshot() AuthSnapshot {
	if p := s.cur.Load(); p != nil {
		return *p
	}

	return AuthSnapshot{}
}

// Close stops the watch. The last published snapshot stays readable.
func (s *AuthSource) Close() error {
	s.cancel()
	s.done.Wait()

	return nil
}

// load fetches the full auth namespace and publishes it, returning the revision
// it reflects.
func (s *AuthSource) load(ctx context.Context) (int64, error) {
	resp, err := s.client.Get(ctx, s.cfg.authPrefix(), clientv3.WithPrefix())
	if err != nil {
		return 0, errors.Wrap(err, "load auth state")
	}

	for _, kv := range resp.Kvs {
		s.applyKV(kv.Key, kv.Value, false)
	}

	s.publish()

	return resp.Header.Revision, nil
}

// watch applies auth events from rev onward, re-establishing the watch (with a
// fresh full load) whenever it breaks.
func (s *AuthSource) watch(ctx context.Context, rev int64) {
	for {
		ch := s.client.Watch(ctx, s.cfg.authPrefix(), clientv3.WithPrefix(), clientv3.WithRev(rev))

		for resp := range ch {
			if err := resp.Err(); err != nil {
				s.reportErr(errors.Wrap(err, "auth watch"))
				break
			}

			for _, ev := range resp.Events {
				s.apply(ev)
			}

			rev = resp.Header.Revision + 1

			s.publish()
		}

		if contextDone(ctx) {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(rewatchBackoff):
		}

		// The watch broke (compaction, leader loss): resync from a full load so
		// no event is missed, then watch from the loaded revision.
		clear(s.records)
		s.publicRead = nil

		loaded, err := s.load(ctx)
		if err != nil {
			s.reportErr(err)
			continue
		}

		rev = loaded + 1
	}
}

// apply folds one auth event into the in-memory state.
func (s *AuthSource) apply(ev *clientv3.Event) {
	s.applyKV(ev.Kv.Key, ev.Kv.Value, ev.Type == clientv3.EventTypeDelete)
}

// applyKV routes one key/value (or deletion) to the credential map or the
// public-read list by its key. Unknown keys under the namespace are ignored, so
// future auth state does not disturb an old node.
func (s *AuthSource) applyKV(key, value []byte, deleted bool) {
	k := string(key)

	switch {
	case strings.HasPrefix(k, s.cfg.authKeysPrefix()):
		accessKey := k[len(s.cfg.authKeysPrefix()):]
		if deleted {
			delete(s.records, accessKey)

			return
		}

		rec, err := decodeAuthRecord(value)
		if err != nil {
			s.reportErr(err)

			return
		}

		s.records[rec.AccessKey] = rec
	case k == s.cfg.publicReadKey():
		if deleted {
			s.publicRead = nil

			return
		}

		buckets, err := decodePublicRead(value)
		if err != nil {
			s.reportErr(err)

			return
		}

		s.publicRead = buckets
	}
}

// publish snapshots the current state (records sorted) and hands it to the
// callback.
func (s *AuthSource) publish() {
	records := make([]AuthRecord, 0, len(s.records))
	for _, rec := range s.records {
		records = append(records, rec)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].AccessKey < records[j].AccessKey })

	snap := AuthSnapshot{Records: records, PublicRead: append([]string(nil), s.publicRead...)}

	s.cur.Store(&snap)
	s.onChange(snap)
}

// reportErr forwards a background error to the hook, if set.
func (s *AuthSource) reportErr(err error) {
	if s.OnError != nil {
		s.OnError(err)
	}
}
