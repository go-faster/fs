package etcd

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// rebalanceElectionPrefix is the election key namespace for the single
// cluster-wide rebalance runner.
func (c Config) rebalanceElectionPrefix() string { return c.Prefix + "/rebalance/leader" }

// rebalanceCursorKey holds the rebalance resume cursor.
func (c Config) rebalanceCursorKey() string { return c.Prefix + "/rebalance/cursor" }

// closeTimeout bounds the etcd calls made while releasing leadership.
const closeTimeout = 5 * time.Second

// RebalanceLeadership is a held cluster-wide rebalance slot: at most one
// candidate per key prefix holds it at a time. Leadership is lease-bound — a
// killed or partitioned holder loses it after the TTL and the next candidate's
// Campaign returns, so a standby runner resumes from the persisted cursor.
type RebalanceLeadership struct {
	session  *concurrency.Session
	election *concurrency.Election
	cursor   string
}

// CampaignRebalance blocks until this candidate holds the cluster-wide
// rebalance leadership or ctx is done. candidate is a diagnostic label
// (e.g. host/pid) stored as the election value.
func CampaignRebalance(ctx context.Context, client *clientv3.Client, cfg Config, candidate string) (*RebalanceLeadership, error) {
	cfg = cfg.withDefaults()

	// The session lives on the client's context, not ctx: leadership must be
	// released deterministically by Close, and lost only through real lease
	// expiry (holder death or partition).
	session, err := concurrency.NewSession(client, concurrency.WithTTL(int(cfg.TTL)))
	if err != nil {
		return nil, errors.Wrap(err, "rebalance election session")
	}

	election := concurrency.NewElection(session, cfg.rebalanceElectionPrefix())

	if err := election.Campaign(ctx, candidate); err != nil {
		_ = session.Close()
		return nil, errors.Wrap(err, "campaign rebalance leadership")
	}

	return &RebalanceLeadership{session: session, election: election, cursor: cfg.rebalanceCursorKey()}, nil
}

// Done is closed when leadership is lost involuntarily (the session lease
// expired); the holder must stop rebalancing immediately.
func (l *RebalanceLeadership) Done() <-chan struct{} { return l.session.Done() }

// SaveCursor persists the resume cursor, fenced on still holding leadership:
// a deposed runner's late write is rejected rather than clobbering the new
// leader's progress.
func (l *RebalanceLeadership) SaveCursor(ctx context.Context, value string) error {
	resp, err := l.session.Client().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(l.election.Key()), "=", l.election.Rev())).
		Then(clientv3.OpPut(l.cursor, value)).
		Commit()
	if err != nil {
		return errors.Wrap(err, "save rebalance cursor")
	}

	if !resp.Succeeded {
		return errors.New("rebalance leadership lost")
	}

	return nil
}

// ClearCursor removes the resume cursor (the walk completed), fenced like
// SaveCursor.
func (l *RebalanceLeadership) ClearCursor(ctx context.Context) error {
	resp, err := l.session.Client().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(l.election.Key()), "=", l.election.Rev())).
		Then(clientv3.OpDelete(l.cursor)).
		Commit()
	if err != nil {
		return errors.Wrap(err, "clear rebalance cursor")
	}

	if !resp.Succeeded {
		return errors.New("rebalance leadership lost")
	}

	return nil
}

// Close resigns leadership and ends the session, letting the next candidate
// win immediately instead of after the TTL.
func (l *RebalanceLeadership) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	err := l.election.Resign(ctx)

	if cerr := l.session.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return errors.Wrap(err, "release rebalance leadership")
	}

	return nil
}

// LoadRebalanceCursor reads the persisted resume cursor; ok is false when no
// rebalance is in progress.
func LoadRebalanceCursor(ctx context.Context, client *clientv3.Client, cfg Config) (value string, ok bool, err error) {
	cfg = cfg.withDefaults()

	resp, err := client.Get(ctx, cfg.rebalanceCursorKey())
	if err != nil {
		return "", false, errors.Wrap(err, "load rebalance cursor")
	}

	if len(resp.Kvs) == 0 {
		return "", false, nil
	}

	return string(resp.Kvs[0].Value), true, nil
}
