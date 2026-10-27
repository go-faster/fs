package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/cluster/table"
)

// Backfill (docs/design/geo-replication.md, "Existing objects"): versions
// written before a target was set have no outbox row for it. A walk of the
// bucket's versions writes one for each, and the runner ships them like any
// other.
//
// Its state is two bucket settings per target, each with one writer: the
// request (setting a new target, or an operator asking again) and the
// walk's progress (the runner). A request newer than the progress starts
// the walk over.

// settingBackfill and settingBackfilled prefix the request and the
// progress settings, followed by the target's name.
const (
	settingBackfill   = "replication.backfill:"
	settingBackfilled = "replication.backfilled:"
)

// staleSettings is how old the bucket settings a write commits with may be;
// a backfill must start later than this after its request. Engine.stale
// holds it, for tests to move.
const staleSettings = 10 * time.Second

// BackfillDelay is how long after its request a backfill starts, so that
// every write committing with settings from before the request has landed.
const BackfillDelay = time.Minute

// BackfillProgress is how far a target's walk has got.
type BackfillProgress struct {
	// Request is the request walked for, its Unix nanoseconds.
	Request int64 `json:"request"`
	// Cursor is the next key to walk; Done when the walk ended.
	Cursor string `json:"cursor,omitempty"`
	Done   bool   `json:"done,omitempty"`
	// Walked counts the versions with content walked, Queued the outbox
	// rows written.
	Walked int64 `json:"walked,omitempty"`
	Queued int64 `json:"queued,omitempty"`
}

// Backfill is a target's backfill.
type Backfill struct {
	// Requested is when one was last asked for; zero when never.
	Requested time.Time
	// Progress is the walk's, of Requested or an earlier request.
	Progress BackfillProgress
	// Remaining estimates the versions with content the walk has not
	// reached: the bucket's versions less those walked.
	Remaining int64
}

// Due reports whether the backfill has a walk to do.
func (b Backfill) Due() bool {
	return !b.Requested.IsZero() && (b.Progress.Request != b.Requested.UnixNano() || !b.Progress.Done)
}

// backfillDue reports whether b's backfill to target has a walk to do.
func backfillDue(b meta.Bucket, target string) bool {
	var (
		req int64
		p   BackfillProgress
	)

	if s, ok := b.Settings[settingBackfill+target]; !ok || json.Unmarshal(s.V, &req) != nil || req <= 0 {
		return false
	}

	if s, ok := b.Settings[settingBackfilled+target]; ok {
		_ = json.Unmarshal(s.V, &p)
	}

	return Backfill{Requested: time.Unix(0, req), Progress: p}.Due()
}

// RequestReplicationBackfill asks for a backfill of a bucket's versions to
// its target: every version stored is queued to ship again, including those
// the target has since deleted. Setting a new target asks for one itself.
func (e *Engine) RequestReplicationBackfill(ctx context.Context, bucket, target string) error {
	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return err
	}

	for _, t := range replicationTargets(b) {
		if t.Name == target {
			return e.setSetting(ctx, bucket, settingBackfill+target, e.ts())
		}
	}

	return errors.Wrapf(fs.ErrInvalidBucketState, "bucket %q has no replication target %q", bucket, target)
}

// ReplicationBackfills returns the backfill of each of a bucket's targets.
func (e *Engine) ReplicationBackfills(ctx context.Context, bucket string) (map[string]Backfill, error) {
	b, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return nil, err
	}

	u, err := e.objects.Usage(ctx, inc.ID)
	if err != nil {
		return nil, err
	}

	out := map[string]Backfill{}

	for _, t := range replicationTargets(b) {
		var (
			req int64
			bf  Backfill
		)

		if s, ok := b.Settings[settingBackfill+t.Name]; ok && json.Unmarshal(s.V, &req) == nil && req > 0 {
			bf.Requested = time.Unix(0, req)
		}

		if s, ok := b.Settings[settingBackfilled+t.Name]; ok {
			_ = json.Unmarshal(s.V, &bf.Progress)
		}

		if bf.Due() {
			walked := bf.Progress.Walked
			if bf.Progress.Request != req {
				walked = 0
			}

			bf.Remaining = max(u.Items-walked, 0)
		}

		out[t.Name] = bf
	}

	return out, nil
}

// SaveBackfillProgress records how far a target's walk has got.
func (e *Engine) SaveBackfillProgress(ctx context.Context, bucket, target string, p BackfillProgress) error {
	return e.setSetting(ctx, bucket, settingBackfilled+target, p)
}

// BackfillReplication walks up to limit keys of a bucket from p's cursor,
// writing for target an outbox row of every version it would ship, and
// returns the progress after them. p is for the request req; a p of an
// earlier request starts over.
func (e *Engine) BackfillReplication(
	ctx context.Context, bucket, target string, req int64, p BackfillProgress, limit int,
) (BackfillProgress, error) {
	if p.Request != req {
		p = BackfillProgress{Request: req}
	}

	if p.Done {
		return p, nil
	}

	b, inc, err := e.bucket(ctx, bucket)
	if err != nil {
		return p, err
	}

	var t *ReplicationTarget

	for _, c := range replicationTargets(b) {
		if c.Name == target {
			t = &c

			break
		}
	}

	if t == nil {
		return p, errors.Wrapf(fs.ErrInvalidBucketState, "bucket %q has no replication target %q", bucket, target)
	}

	page, err := e.objects.RangePrefix(ctx, inc.ID, max(p.Cursor, t.Prefix), t.Prefix, limit)
	if err != nil {
		return p, err
	}

	var rows []table.Row

	for _, r := range page {
		for _, v := range r.Row.Versions {
			if v.State != meta.Complete || (v.DeleteMarker && !t.DeleteMarkers) {
				continue
			}

			var size int64

			if !v.DeleteMarker {
				p.Walked++

				if pl, err := decodePayload(v); err == nil {
					size = pl.Size
				}
			}

			row, err := e.outbox.Row(inc.ID, meta.OutboxSK(r.SK, v.ID), meta.Outbox{
				Targets: []string{target},
				Pending: max(v.TS, v.Attrs.TS),
				Version: true,
				Size:    size,
			})
			if err != nil {
				return p, err
			}

			rows = append(rows, row)
		}
	}

	if len(rows) > 0 {
		if err := table.Write(ctx, e.member, rows...); err != nil {
			return p, err
		}
	}

	p.Queued += int64(len(rows))

	if len(page) < limit {
		p.Cursor, p.Done = "", true
	} else {
		p.Cursor = page[len(page)-1].SK + "\x00"
	}

	return p, nil
}

// requestBackfills asks for a backfill to each target of targets that is new
// against before: a name not there, or one now pointing at another bucket.
func (e *Engine) requestBackfills(ctx context.Context, bucket string, before, targets []ReplicationTarget) error {
	for _, t := range targets {
		fresh := true

		for _, o := range before {
			if o.Name == t.Name && o.Endpoint == t.Endpoint && o.Bucket == t.Bucket && o.Prefix == t.Prefix {
				fresh = false
			}
		}

		if fresh {
			if err := e.setSetting(ctx, bucket, settingBackfill+t.Name, e.ts()); err != nil {
				return err
			}
		}
	}

	return nil
}
