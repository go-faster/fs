package replication

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/engine"
)

// Source is the deployment versions are replicated from.
type Source interface {
	// ReplicatedBuckets returns the buckets with targets, and their targets.
	ReplicatedBuckets(ctx context.Context) (map[string][]engine.ReplicationTarget, error)
	// ReplicationBacklog is what a bucket has still to ship.
	ReplicationBacklog(ctx context.Context, bucket string) (engine.Usage, error)
	// Outbox reads a page of a bucket's outbox.
	Outbox(ctx context.Context, bucket, after string, limit int) ([]engine.OutboxEntry, error)
	// MarkReplicated records deliveries and refusals.
	MarkReplicated(ctx context.Context, bucket string, marks []engine.ReplicationMark) error
	// ExportReplica and ReadReplicaBlock read what is shipped.
	ExportReplica(ctx context.Context, bucket, key, versionID string, attrsOnly bool) (fs.ReplicaVersion, error)
	ReadReplicaBlock(ctx context.Context, b fs.ReplicaBlock) ([]byte, error)
	// ReplicationBackfills, BackfillReplication and SaveBackfillProgress
	// walk a bucket for the targets that asked for a backfill.
	ReplicationBackfills(ctx context.Context, bucket string) (map[string]engine.Backfill, error)
	BackfillReplication(
		ctx context.Context, bucket, target string, req int64, p engine.BackfillProgress, limit int,
	) (engine.BackfillProgress, error)
	SaveBackfillProgress(ctx context.Context, bucket, target string, p engine.BackfillProgress) error
}

var _ Source = (*engine.Engine)(nil)

const (
	// defaultInterval is how often an idle runner looks for changes.
	defaultInterval = 5 * time.Second
	// outboxPage is how many outbox rows a pass reads at a time.
	outboxPage = 500
	// applyBatch is how many versions go to a target in one request.
	applyBatch = 100
	// blocksInFlight is how many blocks of a version are sent at once.
	blocksInFlight = 4
	// maxBackoff caps the wait after a target's failures.
	maxBackoff = time.Minute
	// backfillPage is how many keys a backfill walks at a time, and
	// backfillPages how many pages of a target a pass walks, so that
	// shipping keeps up with the walk.
	backfillPage  = 1000
	backfillPages = 10
	// refusedRetry is how long a refused version waits before it is tried
	// again: the refusal is the target's answer, and it rarely changes soon.
	refusedRetry = time.Minute
)

// Target names a replication target of a bucket.
type Target struct {
	Bucket, Name string
}

// TargetStats is what the runner did for a target, for an operator to watch.
type TargetStats struct {
	// Shipped and Refused count versions delivered and refused since start.
	Shipped, Refused int64
	// BlocksOffered counts blocks of shipped versions, BlocksSent those the
	// target lacked, and BytesSent their bytes: the ratio is the dedupe.
	BlocksOffered, BlocksSent, BytesSent int64
	// Pending and PendingBytes are the versions still to ship at the last
	// pass that read the whole outbox.
	Pending, PendingBytes int64
	// Lag is the age of the oldest change still to ship then; zero when
	// none was.
	Lag time.Duration
	// LastSuccess is when a request to the target last succeeded, and
	// LastFailure when one last failed, with LastError.
	LastSuccess, LastFailure time.Time
	LastError                string
	// Failing is set from a failure until the next success.
	Failing bool
	// Backfilling is set while a backfill is to walk, and BackfillRemaining
	// estimates the versions it has not reached.
	Backfilling       bool
	BackfillRemaining int64
}

// Runner ships the outbox of every bucket it owns to the bucket's targets.
type Runner struct {
	// Source is replicated from. Required.
	Source Source
	// Owns reports whether this runner ships a bucket; nil means every
	// bucket. In a cluster each bucket is given to one node.
	Owns func(bucket string) bool
	// HTTP sends requests to targets; nil means a client with a timeout.
	HTTP *http.Client
	// Log receives a target's first failure after a success, its recovery,
	// and refusals; nil discards them.
	Log *zap.Logger
	// Interval is how often an idle runner looks for changes; zero means
	// five seconds.
	Interval time.Duration
	// BackfillDelay is how long after its request a backfill starts; zero
	// means engine.BackfillDelay.
	BackfillDelay time.Duration

	mu      sync.Mutex
	targets map[Target]*target
}

// target is a target's state in the runner.
type target struct {
	cfg    engine.ReplicationTarget
	client *Client
	// failures counts the failures since the last success; next is when the
	// target is tried again.
	failures int
	next     time.Time
	// retry is when each refused version, by outbox sort key, is tried
	// again.
	retry map[string]time.Time
	stats TargetStats
}

// Runs reports whether this runner ships bucket.
func (r *Runner) Runs(bucket string) bool { return r.Owns == nil || r.Owns(bucket) }

// Stats returns what the runner did, by target. A target of a bucket this
// runner does not own is absent.
func (r *Runner) Stats() map[Target]TargetStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[Target]TargetStats, len(r.targets))
	for k, t := range r.targets {
		out[k] = t.stats
	}

	return out
}

// Run ships until ctx is canceled: a pass, then another at once while
// passes find work, and every Interval otherwise.
func (r *Runner) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	for {
		busy, err := r.Pass(ctx)
		if err != nil && ctx.Err() == nil {
			r.log().Warn("Replication pass failed", zap.Error(err))
		}

		wait := interval
		if busy {
			wait = 0
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Pass ships what every owned bucket has pending, and reports whether it
// shipped anything.
func (r *Runner) Pass(ctx context.Context) (busy bool, err error) {
	buckets, err := r.Source.ReplicatedBuckets(ctx)
	if err != nil {
		return false, errors.Wrap(err, "list replicated buckets")
	}

	live := map[Target]bool{}

	for bucket, targets := range buckets {
		if !r.Runs(bucket) {
			continue
		}

		for _, t := range targets {
			live[Target{bucket, t.Name}] = true
		}

		b, berr := r.bucketPass(ctx, bucket, targets)
		busy = busy || b

		if berr != nil {
			err = errors.Join(err, errors.Wrapf(berr, "bucket %q", bucket))
		}
	}

	// Targets removed, or of buckets this runner no longer owns, are
	// forgotten.
	r.mu.Lock()
	for k := range r.targets {
		if !live[k] {
			delete(r.targets, k)
		}
	}
	r.mu.Unlock()

	return busy, err
}

// scan is what a pass found pending for a target.
type scan struct {
	pending, bytes int64
	oldest         int64
	batch          []engine.OutboxEntry
}

// bucketPass ships one bucket's outbox to its targets.
func (r *Runner) bucketPass(ctx context.Context, bucket string, cfgs []engine.ReplicationTarget) (busy bool, err error) {
	now := time.Now()
	targets := make(map[string]*target, len(cfgs))
	due := map[string]bool{}

	for _, cfg := range cfgs {
		t, err := r.target(bucket, cfg)
		if err != nil {
			return false, err
		}

		targets[cfg.Name] = t

		r.mu.Lock()
		if !now.Before(t.next) {
			due[cfg.Name] = true
		}
		r.mu.Unlock()
	}

	busy, err = r.backfill(ctx, bucket, targets)
	if err != nil {
		return busy, err
	}

	if len(due) == 0 {
		return busy, nil
	}

	// The tally answers "anything at all?" in one read; an idle bucket
	// costs no scan.
	backlog, err := r.Source.ReplicationBacklog(ctx, bucket)
	if err != nil {
		return false, errors.Wrap(err, "read backlog")
	}

	scans := make(map[string]*scan, len(targets))
	for name := range targets {
		scans[name] = &scan{}
	}

	if backlog.Objects > 0 {
		shipped, err := r.scanOutbox(ctx, bucket, targets, due, scans)
		busy = busy || shipped

		if err != nil {
			return busy, err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for name, sc := range scans {
		t := targets[name]
		t.stats.Pending, t.stats.PendingBytes, t.stats.Lag = sc.pending, sc.bytes, 0

		if sc.oldest > 0 {
			t.stats.Lag = max(now.Sub(time.Unix(0, sc.oldest)), 0)
		}
	}

	return busy, nil
}

// backfill walks a page or more of a bucket for every target whose backfill
// is due, writing outbox rows for the pass after it to ship, and reports
// whether it walked anything.
func (r *Runner) backfill(ctx context.Context, bucket string, targets map[string]*target) (busy bool, err error) {
	bfs, err := r.Source.ReplicationBackfills(ctx, bucket)
	if err != nil {
		return false, errors.Wrap(err, "read backfills")
	}

	delay := r.BackfillDelay
	if delay <= 0 {
		delay = engine.BackfillDelay
	}

	for name, bf := range bfs {
		t, ok := targets[name]
		if !ok {
			continue
		}

		if bf.Due() && time.Since(bf.Requested) >= delay {
			p, req := bf.Progress, bf.Requested.UnixNano()

			for range backfillPages {
				before := p.Walked
				if p.Request != req {
					before = 0
				}

				if p, err = r.Source.BackfillReplication(ctx, bucket, name, req, p, backfillPage); err != nil {
					return busy, errors.Wrapf(err, "backfill %q", name)
				}

				if err := r.Source.SaveBackfillProgress(ctx, bucket, name, p); err != nil {
					return busy, errors.Wrapf(err, "save backfill of %q", name)
				}

				busy = true
				bf.Remaining = max(bf.Remaining-(p.Walked-before), 0)

				if p.Done {
					r.log().Info("Replication backfill walked",
						zap.String("bucket", bucket), zap.String("target", name),
						zap.Int64("versions", p.Walked), zap.Int64("queued", p.Queued))

					break
				}
			}

			bf.Progress = p
		}

		r.mu.Lock()
		t.stats.Backfilling, t.stats.BackfillRemaining = bf.Due(), bf.Remaining
		r.mu.Unlock()
	}

	return busy, nil
}

// scanOutbox reads the whole of a bucket's outbox, counting what each target
// has pending and shipping it to the targets that are due. A target that
// fails stops being due for the rest of the pass.
func (r *Runner) scanOutbox(
	ctx context.Context, bucket string, targets map[string]*target, due map[string]bool, scans map[string]*scan,
) (busy bool, err error) {
	now := time.Now()
	after := ""

	for {
		page, err := r.Source.Outbox(ctx, bucket, after, outboxPage)
		if err != nil {
			return busy, errors.Wrap(err, "read outbox")
		}

		// Rows still pending for a target no longer configured are marked
		// shipped, so they do not wait for it.
		var gone []engine.ReplicationMark

		for _, e := range page {
			for _, name := range e.Row.Targets {
				if _, ok := targets[name]; !ok && e.Row.PendingFor(name) {
					gone = append(gone, engine.ReplicationMark{
						Key: e.Key, VersionID: e.VersionID, Target: name, Pending: e.Row.Pending,
					})
				}
			}

			for name, t := range targets {
				if !e.Row.PendingFor(name) {
					continue
				}

				sc := scans[name]
				sc.pending++
				sc.bytes += e.Row.Size

				if sc.oldest == 0 || e.Row.Pending < sc.oldest {
					sc.oldest = e.Row.Pending
				}

				r.mu.Lock()
				wait := t.retry[e.After()]
				r.mu.Unlock()

				if due[name] && !now.Before(wait) {
					sc.batch = append(sc.batch, e)
				}
			}
		}

		if err := r.Source.MarkReplicated(ctx, bucket, gone); err != nil {
			return busy, errors.Wrap(err, "mark removed targets")
		}

		for name, sc := range scans {
			if len(sc.batch) == 0 {
				continue
			}

			busy = true

			if err := r.ship(ctx, bucket, targets[name], sc.batch); err != nil {
				if ctx.Err() != nil {
					return busy, ctx.Err()
				}

				r.failed(bucket, targets[name], err)
				delete(due, name)
			}

			sc.batch = sc.batch[:0]
		}

		if len(page) < outboxPage {
			return busy, nil
		}

		after = page[len(page)-1].After()
	}
}

// target returns the runner's state of a target, the client rebuilt when
// its configuration changed.
func (r *Runner) target(bucket string, cfg engine.ReplicationTarget) (*target, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.targets == nil {
		r.targets = map[Target]*target{}
	}

	k := Target{bucket, cfg.Name}

	t, ok := r.targets[k]
	if ok && t.cfg == cfg {
		return t, nil
	}

	hc := r.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}

	c, err := NewClient(cfg.Endpoint, cfg.Bucket, cfg.AccessKeyID, cfg.SecretAccessKey, hc)
	if err != nil {
		return nil, errors.Wrapf(err, "target %q", cfg.Name)
	}

	if !ok {
		t = &target{retry: map[string]time.Time{}}
		r.targets[k] = t
	}

	// A new configuration is tried at once.
	t.cfg, t.client, t.failures, t.next = cfg, c, 0, time.Time{}

	return t, nil
}

// ship delivers entries, outbox rows pending for t, in batches. An error is
// the target's or the source's, not one version's: the rest waits for the
// backoff.
func (r *Runner) ship(ctx context.Context, bucket string, t *target, entries []engine.OutboxEntry) error {
	for len(entries) > 0 {
		n := min(len(entries), applyBatch)

		if err := r.shipBatch(ctx, bucket, t, entries[:n]); err != nil {
			return err
		}

		entries = entries[n:]
	}

	return nil
}

func (r *Runner) shipBatch(ctx context.Context, bucket string, t *target, entries []engine.OutboxEntry) error {
	var (
		marks    []engine.ReplicationMark
		versions []fs.ReplicaVersion
		sent     []engine.OutboxEntry
	)

	mark := func(e engine.OutboxEntry, refused string) {
		marks = append(marks, engine.ReplicationMark{
			Key: e.Key, VersionID: e.VersionID, Target: t.cfg.Name, Pending: e.Row.Pending, Refused: refused,
		})
	}

	for _, e := range entries {
		// The version itself ships the first time; after that, or for a
		// row an attributes change recreated, its tags and ACL alone.
		attrsOnly := !e.Row.Version || e.Row.Shipped[t.cfg.Name] > 0

		rv, err := r.Source.ExportReplica(ctx, bucket, e.Key, e.VersionID, attrsOnly)

		switch {
		case errors.Is(err, fs.ErrVersionNotFound):
			// Deleted here since: version deletes are not replicated, so
			// there is nothing left to send.
			mark(e, "")

			continue
		case err != nil:
			return errors.Wrapf(err, "export %s@%s", e.Key, e.VersionID)
		}

		if len(rv.Blocks) > 0 {
			reason, err := r.shipBlocks(ctx, t, &rv)
			if err != nil {
				return err
			}

			if reason != "" {
				r.refused(bucket, t, e, reason)
				mark(e, reason)

				continue
			}
		}

		versions = append(versions, rv)
		sent = append(sent, e)
	}

	if len(versions) > 0 {
		results, err := t.client.Apply(ctx, versions)
		if err != nil {
			return errors.Wrap(err, "apply")
		}

		var shipped int64

		for i, res := range results {
			if res.Error != "" {
				r.refused(bucket, t, sent[i], res.Error)
				mark(sent[i], res.Error)

				continue
			}

			shipped++

			mark(sent[i], "")
		}

		r.mu.Lock()
		t.stats.Shipped += shipped
		r.mu.Unlock()
	}

	if err := r.Source.MarkReplicated(ctx, bucket, marks); err != nil {
		return errors.Wrap(err, "mark shipped")
	}

	r.succeeded(bucket, t, marks)

	return nil
}

// shipBlocks claims a version's blocks at the target and sends those it
// lacks, rewriting rv's blocks to the scheme the target stores them with.
// A block this deployment cannot read is the version's problem, returned as
// the reason it was not shipped; anything else is the target's.
func (r *Runner) shipBlocks(ctx context.Context, t *target, rv *fs.ReplicaVersion) (reason string, err error) {
	claims, err := t.client.Claim(ctx, rv.VersionID, rv.Blocks)
	if err != nil {
		return "", errors.Wrapf(err, "claim blocks of %s", rv.VersionID)
	}

	var (
		g       errgroup.Group
		mu      sync.Mutex
		unread  error
		sent, n int64
	)

	g.SetLimit(blocksInFlight)

	for i, c := range claims {
		src := rv.Blocks[i]
		rv.Blocks[i].Scheme = c.Scheme

		if !c.Missing {
			continue
		}

		g.Go(func() error {
			data, err := r.Source.ReadReplicaBlock(ctx, src)
			if err != nil {
				mu.Lock()
				unread = errors.Wrapf(err, "read block %s", src.Hash)
				mu.Unlock()

				return nil
			}

			if err := t.client.PutBlock(ctx, rv.VersionID, fs.ReplicaBlock{Hash: c.Hash, Scheme: c.Scheme}, data); err != nil {
				return errors.Wrapf(err, "send block %s", c.Hash)
			}

			mu.Lock()
			sent++
			n += int64(len(data))
			mu.Unlock()

			return nil
		})
	}

	err = g.Wait()

	r.mu.Lock()
	t.stats.BlocksOffered += int64(len(claims))
	t.stats.BlocksSent += sent
	t.stats.BytesSent += n
	r.mu.Unlock()

	switch {
	case err != nil:
		return "", err
	case unread != nil:
		return "source: " + unread.Error(), nil
	default:
		return "", nil
	}
}

// succeeded records a request round that reached the target.
func (r *Runner) succeeded(bucket string, t *target, marks []engine.ReplicationMark) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, m := range marks {
		if m.Refused == "" {
			delete(t.retry, engine.OutboxEntry{Key: m.Key, VersionID: m.VersionID}.After())
		}
	}

	if t.failures > 0 {
		r.log().Info("Replication target recovered",
			zap.String("bucket", bucket), zap.String("target", t.cfg.Name), zap.Int("failures", t.failures))
	}

	t.failures, t.next = 0, time.Time{}
	t.stats.LastSuccess, t.stats.Failing = time.Now(), false
}

// failed records a failure of the target, and backs it off.
func (r *Runner) failed(bucket string, t *target, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t.failures == 0 {
		r.log().Warn("Replication target failing",
			zap.String("bucket", bucket), zap.String("target", t.cfg.Name), zap.Error(err))
	}

	t.failures++
	t.next = time.Now().Add(backoff(t.failures))
	t.stats.LastFailure, t.stats.LastError, t.stats.Failing = time.Now(), err.Error(), true
}

// refused records a version the target, or this deployment, would not ship.
// The outbox keeps the reason, for the status anyone can read.
func (r *Runner) refused(bucket string, t *target, e engine.OutboxEntry, reason string) {
	r.log().Warn("Replication of a version refused",
		zap.String("bucket", bucket), zap.String("target", t.cfg.Name),
		zap.String("key", e.Key), zap.String("version", e.VersionID), zap.String("reason", reason))

	r.mu.Lock()
	defer r.mu.Unlock()

	t.retry[e.After()] = time.Now().Add(refusedRetry)
	t.stats.Refused++
}

// backoff is the wait after n failures in a row: doubling from a second, up
// to maxBackoff.
func backoff(n int) time.Duration {
	if n > 7 {
		return maxBackoff
	}

	return min(time.Second<<(n-1), maxBackoff)
}

func (r *Runner) log() *zap.Logger {
	if r.Log == nil {
		return zap.NewNop()
	}

	return r.Log
}

// Check is the handshake with a target before it is configured: reachable,
// the credentials accepted with the admin grant, the protocol spoken, and
// versioning enabled on its bucket.
func Check(ctx context.Context, cfg engine.ReplicationTarget, hc *http.Client) error {
	c, err := NewClient(cfg.Endpoint, cfg.Bucket, cfg.AccessKeyID, cfg.SecretAccessKey, hc)
	if err != nil {
		return err
	}

	info, err := c.Info(ctx)
	if err != nil {
		return errors.Wrapf(err, "target %q", cfg.Name)
	}

	switch {
	case info.Protocol != fs.ReplicaProtocol:
		return errors.Errorf("target %q speaks replication protocol %d, not %d", cfg.Name, info.Protocol, fs.ReplicaProtocol)
	case info.Versioning != fs.VersioningEnabled:
		return errors.Errorf("target %q: bucket %q does not have versioning enabled", cfg.Name, cfg.Bucket)
	}

	return nil
}
