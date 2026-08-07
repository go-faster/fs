package etcd

import (
	"context"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// lastRunKey holds when a named periodic pass last completed.
func (c Config) lastRunKey(task string) string { return c.Prefix + "/lastrun/" + task }

// LastRunStore records pass completions in the control plane, so the record
// outlives not just a restart but the node that wrote it.
//
// That is the difference from the single-node file store: a pass whose record
// lives on one node's disk is forgotten when the pass moves — a re-elected
// lifecycle sweeper, or a node replaced entirely — and the new holder would
// start the interval over. In etcd the record belongs to the pass, not to
// whoever ran it last.
type LastRunStore struct {
	client *clientv3.Client
	cfg    Config
}

// NewLastRunStore returns a store backed by client under cfg's prefix.
func NewLastRunStore(client *clientv3.Client, cfg Config) *LastRunStore {
	return &LastRunStore{client: client, cfg: cfg.withDefaults()}
}

// LastRun returns when task last completed; a zero time when it never has, or
// when the record cannot be parsed — which schedules the pass rather than
// skipping it.
func (s *LastRunStore) LastRun(ctx context.Context, task string) (time.Time, error) {
	value, ok, err := loadKey(ctx, s.client, s.cfg.lastRunKey(task), task+" last-run record")
	if err != nil || !ok {
		return time.Time{}, err
	}

	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, nil
	}

	return at, nil
}

// SetLastRun records that task completed at t.
//
// The write is unfenced, unlike the cursor writes: two nodes recording the same
// pass is not a correctness problem the way two nodes moving a resume cursor
// is. The later timestamp wins, and the worst outcome is one skipped pass.
func (s *LastRunStore) SetLastRun(ctx context.Context, task string, t time.Time) error {
	_, err := s.client.Put(ctx, s.cfg.lastRunKey(task), t.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return errors.Wrapf(err, "record %s last run", task)
	}

	return nil
}
