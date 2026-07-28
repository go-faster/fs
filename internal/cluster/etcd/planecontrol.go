package etcd

import (
	"context"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// planeElectionPrefix is the election key namespace for the single cluster-wide
// metadata plane controller.
func (c Config) planeElectionPrefix() string { return c.Prefix + "/metaplane/leader" }

// PlaneLeadership is a held metadata-plane-controller slot: at most one
// candidate holds it at a time.
//
// Its own election, separate from the rebuild and rebalance ones. The
// controller is what notices a failure and reassigns the range that failed, so
// queueing it behind a rebalance that may take hours would mean a cluster
// cannot fail over while it is moving fragments — the two things most likely to
// be happening at once.
type PlaneLeadership struct {
	session  *concurrency.Session
	election *concurrency.Election
}

// CampaignPlane blocks until this candidate holds the metadata plane
// controller leadership or ctx is done. candidate is a diagnostic label stored
// as the election value.
func CampaignPlane(
	ctx context.Context,
	client *clientv3.Client,
	cfg Config,
	candidate string,
) (*PlaneLeadership, error) {
	cfg = cfg.withDefaults()

	// The session lives on the client's context rather than ctx: leadership is
	// released deterministically by Close, and lost only through real lease
	// expiry — holder death or partition.
	session, err := concurrency.NewSession(client, concurrency.WithTTL(int(cfg.TTL)))
	if err != nil {
		return nil, errors.Wrap(err, "metadata plane election session")
	}

	election := concurrency.NewElection(session, cfg.planeElectionPrefix())

	if err := election.Campaign(ctx, candidate); err != nil {
		_ = session.Close()

		return nil, errors.Wrap(err, "campaign metadata plane leadership")
	}

	return &PlaneLeadership{session: session, election: election}, nil
}

// Done is closed when leadership is lost involuntarily. The holder must stop
// reconciling immediately: a deposed controller still writing partitionings is
// a second writer, and the whole reason there is an election is that there is
// meant to be one.
func (l *PlaneLeadership) Done() <-chan struct{} { return l.session.Done() }

// Check reports whether this leadership is still held, by asking etcd rather
// than by trusting the session.
//
// Called before writing a partitioning. Losing a lease is silent from inside,
// so a deposed controller's in-flight write would otherwise land after the new
// one's and undo a failover that had already happened.
//
// It is a check-then-write rather than a fenced transaction, and that is forced
// rather than chosen: the range map is many keys, etcd refuses a transaction
// touching one twice, and SaveRangeMap therefore replaces by difference across
// several operations. So the window narrows rather than closing. What makes
// that acceptable is that the reconciliation is deterministic — two controllers
// reacting to one failure compute the same map, so the race is between
// duplicates rather than between answers.
func (l *PlaneLeadership) Check(ctx context.Context) error {
	resp, err := l.session.Client().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(l.election.Key()), "=", l.election.Rev())).
		Commit()
	if err != nil {
		return errors.Wrap(err, "check metadata plane leadership")
	}

	if !resp.Succeeded {
		return errors.New("metadata plane leadership lost")
	}

	return nil
}

// Close releases leadership.
func (l *PlaneLeadership) Close() error {
	if err := l.session.Close(); err != nil {
		return errors.Wrap(err, "close metadata plane election session")
	}

	return nil
}
