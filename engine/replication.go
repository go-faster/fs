package engine

import (
	"context"
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/cluster/table"
)

// Geo-replication (docs/design/geo-replication.md): a bucket's versions are
// shipped to buckets of other fs deployments, its replication targets.
//
// What is still to ship is the replication table, the outbox: every change a
// target must receive — a completed version, a delete marker, a change of a
// version's tags or ACL — writes the version's outbox row in the same commit
// as the change itself. Version deletes and the lifecycle sweep's changes
// write none: S3 replicates neither.

// settingReplication is the bucket setting holding its replication targets.
const settingReplication = "replication"

// ReplicationTarget is a bucket on another fs deployment that a bucket's
// versions are replicated to.
type ReplicationTarget struct {
	// Name identifies the target within the bucket.
	Name string `json:"name"`
	// Endpoint is the target deployment's S3 endpoint.
	Endpoint string `json:"endpoint"`
	// Bucket is the target bucket.
	Bucket string `json:"bucket"`
	// AccessKeyID and SecretAccessKey sign requests to the target; the key
	// needs the admin grant on the target bucket.
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	// Prefix limits replication to the keys that start with it.
	Prefix string `json:"prefix,omitempty"`
	// DeleteMarkers replicates delete markers too.
	DeleteMarkers bool `json:"delete_markers,omitempty"`
	// Insecure allows an http:// endpoint. Replication carries data keys,
	// so this is for tests.
	Insecure bool `json:"insecure,omitempty"`
}

// Validate checks t on its own; validTargets checks a bucket's set.
func (t ReplicationTarget) Validate() error {
	switch {
	case t.Name == "":
		return errors.New("a target needs a name")
	case t.Bucket == "":
		return errors.Errorf("target %q: no bucket", t.Name)
	case t.AccessKeyID == "" || t.SecretAccessKey == "":
		return errors.Errorf("target %q: no credentials", t.Name)
	}

	u, err := url.Parse(t.Endpoint)
	if err != nil {
		return errors.Wrapf(err, "target %q: endpoint", t.Name)
	}

	if u.Host == "" {
		return errors.Errorf("target %q: endpoint %q has no host", t.Name, t.Endpoint)
	}

	switch u.Scheme {
	case "https":
	case "http":
		if !t.Insecure {
			return errors.Errorf("target %q: endpoint %q is not https; replication carries data keys", t.Name, t.Endpoint)
		}
	default:
		return errors.Errorf("target %q: endpoint %q is not http(s)", t.Name, t.Endpoint)
	}

	return nil
}

func validTargets(targets []ReplicationTarget) error {
	seen := make(map[string]bool, len(targets))

	for _, t := range targets {
		if err := t.Validate(); err != nil {
			return err
		}

		if seen[t.Name] {
			return errors.Errorf("target %q named twice", t.Name)
		}

		seen[t.Name] = true
	}

	return nil
}

// replicationTargets returns a bucket's replication targets.
func replicationTargets(b meta.Bucket) []ReplicationTarget {
	var targets []ReplicationTarget

	if s, ok := b.Settings[settingReplication]; ok {
		_ = json.Unmarshal(s.V, &targets)
	}

	return targets
}

// BucketReplication returns a bucket's replication targets, secrets
// included.
func (e *Engine) BucketReplication(ctx context.Context, bucket string) ([]ReplicationTarget, error) {
	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return nil, err
	}

	return replicationTargets(b), nil
}

// SetBucketReplication replaces a bucket's replication targets; none stops
// replication. A target new to the bucket, or now pointing elsewhere, is
// backfilled. The bucket must have versioning enabled: S3 replicates
// versions, and so does fs.
func (e *Engine) SetBucketReplication(ctx context.Context, bucket string, targets []ReplicationTarget) error {
	if err := validTargets(targets); err != nil {
		return errors.Wrap(fs.ErrInvalidBucketState, err.Error())
	}

	if len(targets) == 0 {
		return e.setSetting(ctx, bucket, settingReplication, nil)
	}

	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return err
	}

	if versioning(b) != fs.VersioningEnabled {
		return errors.Wrap(fs.ErrInvalidBucketState, "replication needs versioning enabled")
	}

	if err := e.setSetting(ctx, bucket, settingReplication, targets); err != nil {
		return err
	}

	// A new target is backfilled with what the bucket already holds.
	return e.requestBackfills(ctx, bucket, replicationTargets(b), targets)
}

// outboxRow returns the outbox row recording a change to v, a version of key,
// for the targets of b it is for; none when it is for no target. version
// is set when v itself is to ship, clear for a change of its attributes
// alone; ts is the change's.
func (e *Engine) outboxRow(
	ctx context.Context, b meta.Bucket, bucketID, key string, v meta.Version, version bool, ts int64,
) ([]table.Row, error) {
	if fs.IsLifecycleAction(ctx) {
		return nil, nil
	}

	var names []string

	for _, t := range replicationTargets(b) {
		if !strings.HasPrefix(key, t.Prefix) || (v.DeleteMarker && !t.DeleteMarkers) {
			continue
		}

		names = append(names, t.Name)
	}

	if len(names) == 0 {
		return nil, nil
	}

	slices.Sort(names)

	var size int64

	if p, err := decodePayload(v); err == nil {
		size = p.Size
	}

	r, err := e.outbox.Row(bucketID, meta.OutboxSK(key, v.ID), meta.Outbox{
		Targets: names,
		Pending: ts,
		Version: version,
		Size:    size,
	})
	if err != nil {
		return nil, err
	}

	return []table.Row{r}, nil
}

// outboxUsage is the replication table's tally: the versions still to ship
// to some target, and their bytes.
func outboxUsage(o meta.Outbox) table.Usage {
	if o.Done() {
		return table.Usage{}
	}

	return table.Usage{Items: 1, Bytes: o.Size}
}

// ReplicationBacklog is what a bucket has still to ship to its targets: the
// versions that some target has yet to receive, and their bytes.
func (e *Engine) ReplicationBacklog(ctx context.Context, bucket string) (Usage, error) {
	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return Usage{}, err
	}

	u, err := e.outbox.Usage(ctx, inc.ID)
	if err != nil {
		return Usage{}, err
	}

	return usageFrom(u), nil
}

// ReplicationBacklogs is ReplicationBacklog of every bucket with
// replication targets, by name.
func (e *Engine) ReplicationBacklogs(ctx context.Context) (map[string]Usage, error) {
	out := map[string]Usage{}

	err := e.replicatedBuckets(ctx, func(name string, _ meta.Bucket, inc meta.Incarnation) error {
		u, err := e.outbox.Usage(ctx, inc.ID)
		if err != nil {
			return errors.Wrapf(err, "bucket %q", name)
		}

		out[name] = usageFrom(u)

		return nil
	})

	return out, err
}

// ReplicatedBuckets returns every bucket with replication targets, and its
// targets, secrets included.
func (e *Engine) ReplicatedBuckets(ctx context.Context) (map[string][]ReplicationTarget, error) {
	out := map[string][]ReplicationTarget{}

	err := e.replicatedBuckets(ctx, func(name string, b meta.Bucket, _ meta.Incarnation) error {
		out[name] = replicationTargets(b)

		return nil
	})

	return out, err
}

// replicatedBuckets calls fn for every live bucket with replication targets.
func (e *Engine) replicatedBuckets(ctx context.Context, fn func(name string, b meta.Bucket, inc meta.Incarnation) error) error {
	start := ""

	for {
		page, err := e.buckets.Range(ctx, bucketsPK, start, 1000)
		if err != nil {
			return err
		}

		for _, r := range page {
			inc, ok := r.Row.Live()
			if !ok || len(replicationTargets(r.Row)) == 0 {
				continue
			}

			if err := fn(r.SK, r.Row, inc); err != nil {
				return err
			}
		}

		if len(page) < 1000 {
			return nil
		}

		start = page[len(page)-1].SK + "\x00"
	}
}

// OutboxEntry is the outbox row of one version.
type OutboxEntry struct {
	Key, VersionID string
	Row            meta.Outbox
}

// After is the cursor of Outbox that continues after e.
func (e OutboxEntry) After() string { return meta.OutboxSK(e.Key, e.VersionID) + "\x00" }

// Outbox returns up to limit rows of a bucket's outbox, in key order, from
// the cursor after on: "" for the first, OutboxEntry.After for the next. It
// returns rows every target has received too, until they are collected.
func (e *Engine) Outbox(ctx context.Context, bucket, after string, limit int) ([]OutboxEntry, error) {
	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return nil, err
	}

	page, err := e.outbox.Range(ctx, inc.ID, after, limit)
	if err != nil {
		return nil, err
	}

	out := make([]OutboxEntry, 0, len(page))

	for _, r := range page {
		key, id, ok := strings.Cut(r.SK, "\x00")
		if !ok {
			continue
		}

		out = append(out, OutboxEntry{Key: key, VersionID: id, Row: r.Row})
	}

	return out, nil
}

// ReplicationMark records a delivery of a version's change to a target, or
// its refusal.
type ReplicationMark struct {
	Key, VersionID, Target string
	// Pending is the change delivered or refused: the outbox row's Pending
	// when it was read.
	Pending int64
	// Refused is the target's reason; empty for a delivery.
	Refused string
}

// MarkReplicated records deliveries and refusals in a bucket's outbox.
func (e *Engine) MarkReplicated(ctx context.Context, bucket string, marks []ReplicationMark) error {
	if len(marks) == 0 {
		return nil
	}

	_, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return err
	}

	at := e.ts()
	merged := map[string]meta.Outbox{}

	for _, m := range marks {
		o := meta.Outbox{Shipped: map[string]int64{m.Target: m.Pending}}
		if m.Refused != "" {
			o = meta.Outbox{Refused: map[string]meta.Refusal{m.Target: {Pending: m.Pending, At: at, Reason: m.Refused}}}
		}

		sk := meta.OutboxSK(m.Key, m.VersionID)
		merged[sk] = meta.MergeOutbox(merged[sk], o)
	}

	rows := make([]table.Row, 0, len(merged))

	for _, sk := range slices.Sorted(maps.Keys(merged)) {
		r, err := e.outbox.Row(inc.ID, sk, merged[sk])
		if err != nil {
			return err
		}

		rows = append(rows, r)
	}

	return table.Write(ctx, e.member, rows...)
}

// TargetBacklog is what a bucket has still to ship to one target.
type TargetBacklog struct {
	// Pending and Bytes are the versions with a change still to ship, and
	// their size.
	Pending, Bytes int64
	// Oldest is when the oldest of those changes was made; zero when none.
	Oldest time.Time
	// Refused counts the pending versions whose latest change the target
	// refused, and Recent are the latest refusals, newest first.
	Refused int64
	Recent  []TargetRefusal
}

// TargetRefusal is a target's refusal of a version.
type TargetRefusal struct {
	Key, VersionID, Reason string
	At                     time.Time
}

// ReplicationStatus reads a bucket's outbox, up to limit rows, and returns
// what each target named in it has still to ship. complete reports whether
// the whole outbox was read.
func (e *Engine) ReplicationStatus(
	ctx context.Context, bucket string, limit, recent int,
) (out map[string]TargetBacklog, complete bool, err error) {
	targets, err := e.BucketReplication(ctx, bucket)
	if err != nil {
		return nil, false, err
	}

	out = map[string]TargetBacklog{}
	for _, t := range targets {
		out[t.Name] = TargetBacklog{}
	}

	after, read := "", 0

	for {
		n := min(1000, limit-read)
		if n <= 0 {
			break
		}

		page, err := e.Outbox(ctx, bucket, after, n)
		if err != nil {
			return nil, false, err
		}

		for _, en := range page {
			for _, name := range en.Row.Targets {
				if !en.Row.PendingFor(name) {
					continue
				}

				tb := out[name]
				tb.Pending++
				tb.Bytes += en.Row.Size

				if at := time.Unix(0, en.Row.Pending); tb.Oldest.IsZero() || at.Before(tb.Oldest) {
					tb.Oldest = at
				}

				if r, ok := en.Row.RefusedBy(name); ok {
					tb.Refused++
					tb.Recent = append(tb.Recent, TargetRefusal{
						Key: en.Key, VersionID: en.VersionID, Reason: r.Reason, At: time.Unix(0, r.At),
					})
				}

				out[name] = tb
			}
		}

		read += len(page)

		if len(page) < n {
			complete = true

			break
		}

		after = page[len(page)-1].After()
	}

	for name, tb := range out {
		slices.SortFunc(tb.Recent, func(a, b TargetRefusal) int { return b.At.Compare(a.At) })

		if len(tb.Recent) > recent {
			tb.Recent = tb.Recent[:recent]
		}

		out[name] = tb
	}

	return out, complete, nil
}
