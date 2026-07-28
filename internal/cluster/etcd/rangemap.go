package etcd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// rangeMapPrefix holds the metadata plane's partitioning, one key per range.
//
// One key per range rather than one key holding the map, and the first reason
// is a hard limit rather than a preference: etcd's default max-request-bytes is
// ~1.5 MB, and at the exascale target the map is ~8 MB across ~40,000 ranges.
// A single value would not fit, and hand-chunking it is a worse version of what
// a key prefix already gives.
//
// The second reason is that an ownership change is then one key write instead
// of a read-modify-write of the whole map, so splits and moves do not serialize
// against each other.
func (c Config) rangeMapPrefix() string { return c.Prefix + "/meta/ranges/" }

// rangeKey names a range by its start key.
//
// Hex-encoded because a range boundary is arbitrary bytes — it is a position in
// the object key space, which holds anything a bucket name and object key can —
// and etcd keys are compared bytewise. Hex keeps them printable for an operator
// reading the keyspace, and preserves order, which is what makes a prefix read
// return the ranges already sorted.
func (c Config) rangeKey(start string) string {
	return c.rangeMapPrefix() + hex.EncodeToString([]byte(start))
}

// storedRange is a range as persisted. The start key is the etcd key, so it is
// not repeated in the value.
//
// End is hex, for the same reason the key is: a boundary is a position in the
// object key space and can hold any byte, but JSON strings must be valid UTF-8
// — encoding/json silently replaces what is not with U+FFFD. Stored raw, a
// boundary containing 0xFF comes back as a *different* boundary, which is a
// gap or an overlap in a map that still looks well-formed.
type storedRange struct {
	End       string   `json:"end"`
	Owner     string   `json:"owner"`
	Followers []string `json:"followers,omitempty"`
}

// SaveRangeMap writes the whole map, replacing whatever was there.
//
// One transaction, so a reader never sees a half-written partition — which
// would be a map with a gap, and a gap is a key nothing owns. It is the
// initialization path and the repair path; ordinary ownership changes are
// single-key writes through SaveRange and do not come through here.
//
// It replaces by difference rather than by wiping the prefix first, because
// etcd refuses a transaction that touches the same key twice and a prefix
// delete collides with every put under it. So the stale keys are computed and
// deleted, and only they — which also means an unchanged range is not
// needlessly rewritten.
//
// The read-then-write is not fenced. This is the initialization and repair
// path and the caller is expected to hold the relevant leadership; two
// controllers replacing the whole partitioning at once is not a race to
// arbitrate, it is a bug upstream.
func SaveRangeMap(ctx context.Context, client *clientv3.Client, cfg Config, m *rangemap.Map) error {
	if err := m.Validate(); err != nil {
		return errors.Wrap(err, "refusing to save an invalid range map")
	}

	cfg = cfg.withDefaults()

	existing, err := client.Get(ctx, cfg.rangeMapPrefix(), clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return errors.Wrap(err, "read range map")
	}

	ops := make([]clientv3.Op, 0, len(m.Ranges)+len(existing.Kvs))
	wanted := make(map[string]struct{}, len(m.Ranges))

	for _, r := range m.Ranges {
		followers := make([]string, 0, len(r.Followers))
		for _, f := range r.Followers {
			followers = append(followers, string(f))
		}

		value, err := json.Marshal(storedRange{
			End:       hex.EncodeToString([]byte(r.End)),
			Owner:     string(r.Owner),
			Followers: followers,
		})
		if err != nil {
			return errors.Wrap(err, "encode range")
		}

		key := cfg.rangeKey(r.Start)
		wanted[key] = struct{}{}

		ops = append(ops, clientv3.OpPut(key, string(value)))
	}

	// Boundaries that no longer exist. Left behind they would overlap their
	// successor, and an overlap is two nodes each believing they own the same
	// keys — which Validate would catch on the next load, after the damage.
	for _, kv := range existing.Kvs {
		if _, keep := wanted[string(kv.Key)]; !keep {
			ops = append(ops, clientv3.OpDelete(string(kv.Key)))
		}
	}

	if _, err := client.Txn(ctx).Then(ops...).Commit(); err != nil {
		return errors.Wrap(err, "save range map")
	}

	return nil
}

// LoadRangeMap reads the partitioning, and the revision it was read at.
//
// ok is false when no map has been written — a cluster whose metadata plane has
// not been initialized, which is worth telling apart from an empty or broken
// one.
//
// The revision comes back on the map because it is what makes routing
// self-correcting: a request carries the revision it was routed with, and a
// node holding a newer one rejects it rather than serving from a map it knows
// is stale. Nothing watches this prefix, deliberately — a watch from every node
// on a 40,000-key prefix is the etcd fan-out the design exists to avoid.
func LoadRangeMap(
	ctx context.Context,
	client *clientv3.Client,
	cfg Config,
) (m *rangemap.Map, ok bool, err error) {
	cfg = cfg.withDefaults()

	resp, err := client.Get(ctx, cfg.rangeMapPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, false, errors.Wrap(err, "load range map")
	}

	if len(resp.Kvs) == 0 {
		return nil, false, nil
	}

	out := &rangemap.Map{Revision: resp.Header.Revision}

	for _, kv := range resp.Kvs {
		start, err := hex.DecodeString(string(kv.Key[len(cfg.rangeMapPrefix()):]))
		if err != nil {
			return nil, false, errors.Wrapf(err, "decode range key %q", kv.Key)
		}

		var stored storedRange
		if err := json.Unmarshal(kv.Value, &stored); err != nil {
			return nil, false, errors.Wrapf(err, "decode range %q", kv.Key)
		}

		// Left nil when there are none, rather than an empty slice: a range
		// with no followers must round-trip as the value it was written as, or
		// every caller comparing two maps has to know the difference.
		var followers []cluster.NodeID
		for _, f := range stored.Followers {
			followers = append(followers, cluster.NodeID(f))
		}

		end, err := hex.DecodeString(stored.End)
		if err != nil {
			return nil, false, errors.Wrapf(err, "decode range end for %q", kv.Key)
		}

		out.Ranges = append(out.Ranges, rangemap.Range{
			Start:     string(start),
			End:       string(end),
			Owner:     cluster.NodeID(stored.Owner),
			Followers: followers,
		})
	}

	// Hex preserves order, so a prefix read already returns them sorted — but
	// sorting here means a hand-edited or migrated key cannot produce a map
	// that silently looks partitioned and is not.
	sort.Slice(out.Ranges, func(i, j int) bool { return out.Ranges[i].Start < out.Ranges[j].Start })

	if err := out.Validate(); err != nil {
		return nil, false, errors.Wrap(err, "stored range map is not a partition")
	}

	return out, true, nil
}

// SaveRange writes one range's ownership, fenced on the revision the caller
// last read the map at.
//
// This is the ordinary change: a promotion, a move, a follower set being
// updated. Fencing on the revision is what stops two controllers racing — a
// caller acting on a map it read before someone else changed it is told so
// rather than overwriting them.
func SaveRange(
	ctx context.Context,
	client *clientv3.Client,
	cfg Config,
	r rangemap.Range,
	readAt int64,
) error {
	cfg = cfg.withDefaults()

	followers := make([]string, 0, len(r.Followers))
	for _, f := range r.Followers {
		followers = append(followers, string(f))
	}

	value, err := json.Marshal(storedRange{
		End:       hex.EncodeToString([]byte(r.End)),
		Owner:     string(r.Owner),
		Followers: followers,
	})
	if err != nil {
		return errors.Wrap(err, "encode range")
	}

	key := cfg.rangeKey(r.Start)

	resp, err := client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "<", readAt+1)).
		Then(clientv3.OpPut(key, string(value))).
		Commit()
	if err != nil {
		return errors.Wrap(err, "save range")
	}

	if !resp.Succeeded {
		return errors.Errorf("range %q changed since revision %d", r.Start, readAt)
	}

	return nil
}
