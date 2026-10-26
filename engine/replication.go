package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

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

// validate checks t on its own; validTargets checks a bucket's set.
func (t ReplicationTarget) validate() error {
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
		if err := t.validate(); err != nil {
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
// replication. The bucket must have versioning enabled: S3 replicates
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

	return e.setSetting(ctx, bucket, settingReplication, targets)
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
	start := ""

	for {
		page, err := e.buckets.Range(ctx, bucketsPK, start, 1000)
		if err != nil {
			return nil, err
		}

		for _, r := range page {
			inc, ok := r.Row.Live()
			if !ok || len(replicationTargets(r.Row)) == 0 {
				continue
			}

			u, err := e.outbox.Usage(ctx, inc.ID)
			if err != nil {
				return nil, errors.Wrapf(err, "bucket %q", r.SK)
			}

			out[r.SK] = usageFrom(u)
		}

		if len(page) < 1000 {
			return out, nil
		}

		start = page[len(page)-1].SK + "\x00"
	}
}
