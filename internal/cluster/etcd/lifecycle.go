package etcd

import (
	"context"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// lifecycleElectionPrefix is the election key namespace for the single
// cluster-wide lifecycle sweeper.
func (c Config) lifecycleElectionPrefix() string { return c.Prefix + "/lifecycle/leader" }

// LifecycleLeadership is a held cluster-wide lifecycle-sweep slot: at most one
// candidate holds it at a time. Leadership is lease-bound, so a killed or
// partitioned holder loses it after the TTL and a standby takes over.
//
// It carries no cursor, unlike the rebalance and rebuild elections. A sweep
// pass is cheap to restart — it re-lists what is there and re-derives every
// expiry from the objects themselves — so a new leader starting over costs one
// extra listing rather than the hours a rebuild would lose.
type LifecycleLeadership struct {
	session  *concurrency.Session
	election *concurrency.Election
}

// CampaignLifecycle blocks until this candidate holds the cluster-wide
// lifecycle sweep leadership or ctx is done. candidate is a diagnostic label
// (e.g. host/pid) stored as the election value.
//
// The election is what keeps every node from sweeping the same buckets: the
// pass walks whole buckets and deletes through the ordinary path, so running it
// everywhere would multiply the listing cost by the node count and have every
// node race the others to delete the same keys.
func CampaignLifecycle(
	ctx context.Context,
	client *clientv3.Client,
	cfg Config,
	candidate string,
) (*LifecycleLeadership, error) {
	cfg = cfg.withDefaults()

	// The session lives on the client's context, not ctx: leadership must be
	// released deterministically by Close, and lost only through real lease
	// expiry (holder death or partition).
	session, err := concurrency.NewSession(client, concurrency.WithTTL(int(cfg.TTL)))
	if err != nil {
		return nil, errors.Wrap(err, "lifecycle election session")
	}

	election := concurrency.NewElection(session, cfg.lifecycleElectionPrefix())

	if err := election.Campaign(ctx, candidate); err != nil {
		_ = session.Close()

		return nil, errors.Wrap(err, "campaign lifecycle leadership")
	}

	return &LifecycleLeadership{session: session, election: election}, nil
}

// Done is closed when leadership is lost involuntarily (the session lease
// expired); the holder must stop sweeping immediately.
func (l *LifecycleLeadership) Done() <-chan struct{} { return l.session.Done() }

// Close resigns and releases the session.
func (l *LifecycleLeadership) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	err := l.election.Resign(ctx)

	if cerr := l.session.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return errors.Wrap(err, "release lifecycle leadership")
	}

	return nil
}
