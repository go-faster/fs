package engine

import (
	"context"
	"encoding/json"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/meta"
)

// keysPartition is the one partition every access key lives in.
const keysPartition = "keys"

// PutAccessKey stores an access key's record, replacing any earlier one of the
// same ID — a deleted one included. The record is the caller's: the engine
// only replicates it.
func (e *Engine) PutAccessKey(ctx context.Context, id string, record json.RawMessage) error {
	return e.writeAccessKey(ctx, id, record)
}

// DeleteAccessKey records that an access key is gone. Deleting one that does
// not exist is not an error: the tombstone says the same either way.
func (e *Engine) DeleteAccessKey(ctx context.Context, id string) error {
	return e.writeAccessKey(ctx, id, nil)
}

func (e *Engine) writeAccessKey(ctx context.Context, id string, record json.RawMessage) error {
	if id == "" {
		return errors.New("empty access key ID")
	}

	// Read first so the write orders after what any replica holds: a clock
	// behind the last writer's would otherwise lose to it.
	cur, _, err := e.keys.Get(ctx, keysPartition, id)
	if err != nil {
		return errors.Wrap(err, "read access key")
	}

	row := meta.LWW[json.RawMessage]{TS: meta.NextTS(e.ts(), cur.TS), V: record}

	if err := e.keys.Insert(ctx, keysPartition, id, row); err != nil {
		return errors.Wrap(err, "write access key")
	}

	return nil
}

// AccessKeys returns every live access key's record, by ID.
func (e *Engine) AccessKeys(ctx context.Context) (map[string]json.RawMessage, error) {
	const page = 1000

	keys := map[string]json.RawMessage{}

	for start := ""; ; {
		rows, err := e.keys.Range(ctx, keysPartition, start, page)
		if err != nil {
			return nil, errors.Wrap(err, "list access keys")
		}

		for _, r := range rows {
			if len(r.Row.V) > 0 && string(r.Row.V) != "null" {
				keys[r.SK] = r.Row.V
			}
		}

		if len(rows) < page {
			return keys, nil
		}

		start = rows[len(rows)-1].SK + "\x00"
	}
}
