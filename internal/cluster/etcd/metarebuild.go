package etcd

import (
	"context"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// metaRebuildElectionPrefix is the election key namespace for the single
// cluster-wide metadata rebuild runner.
func (c Config) metaRebuildElectionPrefix() string { return c.Prefix + "/metarebuild/leader" }

// metaRebuildCursorKey holds the metadata rebuild resume cursor.
//
// Its presence is load-bearing beyond resuming: it is what tells a runner that
// a rebuild is already in progress and must be continued rather than started.
// A runner that restarted instead would empty the store and discard whatever
// the previous one had written, which on a cluster large enough to need a
// cursor is a rebuild that never finishes.
func (c Config) metaRebuildCursorKey() string { return c.Prefix + "/metarebuild/cursor" }

// MetaRebuildLeadership is a held cluster-wide metadata-rebuild slot: at most
// one candidate holds it at a time. Leadership is lease-bound — a killed or
// partitioned holder loses it after the TTL and the next candidate's Campaign
// returns, so a standby resumes from the persisted cursor.
type MetaRebuildLeadership struct {
	session  *concurrency.Session
	election *concurrency.Election
	cursor   string
}

// CampaignMetaRebuild blocks until this candidate holds the cluster-wide
// metadata rebuild leadership or ctx is done. candidate is a diagnostic label
// (e.g. host/pid) stored as the election value.
//
// It is a separate election from the rebalance and usage ones. A cluster
// rebuilding its metadata plane is one that cannot serve listings from it, so
// the rebuild must not queue behind a fragment relocation that may take hours.
func CampaignMetaRebuild(
	ctx context.Context,
	client *clientv3.Client,
	cfg Config,
	candidate string,
) (*MetaRebuildLeadership, error) {
	cfg = cfg.withDefaults()

	// The session lives on the client's context, not ctx: leadership must be
	// released deterministically by Close, and lost only through real lease
	// expiry (holder death or partition).
	session, err := concurrency.NewSession(client, concurrency.WithTTL(int(cfg.TTL)))
	if err != nil {
		return nil, errors.Wrap(err, "metadata rebuild election session")
	}

	election := concurrency.NewElection(session, cfg.metaRebuildElectionPrefix())

	if err := election.Campaign(ctx, candidate); err != nil {
		_ = session.Close()

		return nil, errors.Wrap(err, "campaign metadata rebuild leadership")
	}

	return &MetaRebuildLeadership{
		session:  session,
		election: election,
		cursor:   cfg.metaRebuildCursorKey(),
	}, nil
}

// Done is closed when leadership is lost involuntarily (the session lease
// expired); the holder must stop rebuilding immediately.
func (l *MetaRebuildLeadership) Done() <-chan struct{} { return l.session.Done() }

// SaveCursor persists the resume cursor, fenced on still holding leadership: a
// deposed runner's late write is rejected rather than clobbering the new
// leader's progress — which here would move the cursor *backwards* and re-index
// everything between.
func (l *MetaRebuildLeadership) SaveCursor(ctx context.Context, value string) error {
	return l.fenced(ctx, clientv3.OpPut(l.cursor, value), "save metadata rebuild cursor")
}

// ClearCursor removes the resume cursor, fenced like SaveCursor.
//
// Clearing means "no rebuild is in progress", so the next one starts from
// scratch and empties the store first. It must therefore happen only after the
// walk has completed and the store has been marked ready — clearing early would
// make a subsequent runner restart a rebuild that had in fact finished.
func (l *MetaRebuildLeadership) ClearCursor(ctx context.Context) error {
	return l.fenced(ctx, clientv3.OpDelete(l.cursor), "clear metadata rebuild cursor")
}

// fenced runs op only while this leadership is still held.
func (l *MetaRebuildLeadership) fenced(ctx context.Context, op clientv3.Op, what string) error {
	resp, err := l.session.Client().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(l.election.Key()), "=", l.election.Rev())).
		Then(op).
		Commit()
	if err != nil {
		return errors.Wrap(err, what)
	}

	if !resp.Succeeded {
		return errors.New("metadata rebuild leadership lost")
	}

	return nil
}

// Close resigns leadership and ends the session, letting the next candidate win
// immediately instead of after the TTL.
func (l *MetaRebuildLeadership) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	err := l.election.Resign(ctx)

	if cerr := l.session.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return errors.Wrap(err, "release metadata rebuild leadership")
	}

	return nil
}

// LoadMetaRebuildCursor reads the persisted resume cursor; ok is false when no
// rebuild is in progress, which is what tells a runner to start a fresh one.
func LoadMetaRebuildCursor(
	ctx context.Context,
	client *clientv3.Client,
	cfg Config,
) (value string, ok bool, err error) {
	return loadKey(ctx, client, cfg.withDefaults().metaRebuildCursorKey(), "metadata rebuild cursor")
}
