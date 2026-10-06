package table

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
)

// Writing rows of several tables at once: every node applies the rows it
// holds in one transaction, so a write that touches several tables costs
// each node one commit — one fsync pair — instead of one per table, and its
// rows land together or not at all. Each row still needs a quorum of its own
// replicas.

// Row is one row to merge, built by Table.Row for Write.
type Row struct {
	t     writer
	entry wireEntry
}

// writer is a table as Write sees it, whatever its row type.
type writer interface {
	tableName() string
	insertTx(tx *bbolt.Tx, entries []wireEntry) error
	writeSets(pk string) ([][]layout.NodeID, error)
	database() *bbolt.DB
}

// Row returns the row at (pk, sk) for Write.
func (t *Table[R]) Row(pk, sk string, row R) (Row, error) {
	b, err := json.Marshal(row)
	if err != nil {
		return Row{}, errors.Wrap(err, "encode row")
	}

	return Row{t: t, entry: wireEntry{PK: pk, SK: sk, Row: b}}, nil
}

func (t *Table[R]) tableName() string   { return t.name }
func (t *Table[R]) database() *bbolt.DB { return t.db }

// registry is the tables of one member, by name, for the write endpoint.
type registry struct {
	mu     sync.Mutex
	db     *bbolt.DB
	tables map[string]writer
}

//nolint:gochecknoglobals // One registry per member, created with its first table.
var registries sync.Map // *peer.Member → *registry

// register adds t to its member's registry, serving the write endpoint on
// the member's first table.
func register(member *peer.Member, t writer) {
	v, loaded := registries.LoadOrStore(member, &registry{db: t.database(), tables: map[string]writer{}})
	r := v.(*registry) //nolint:forcetypeassert // Only registries are stored.

	r.mu.Lock()
	r.tables[t.tableName()] = t
	r.mu.Unlock()

	if !loaded {
		member.Handle("POST /v1/tables/write", handle(r.serveWrite))
	}
}

type (
	tableEntry struct {
		Table string `json:"table"`
		wireEntry
	}
	writeReq struct {
		Entries []tableEntry `json:"entries"`
	}
)

func (r *registry) serveWrite(req writeReq) (any, error) {
	return nil, r.apply(req.Entries)
}

// apply merges entries of any of the member's tables in one transaction.
// Batch coalesces concurrent writes, and may run fn more than once —
// harmless, merging is idempotent.
func (r *registry) apply(entries []tableEntry) error {
	byTable := map[string][]wireEntry{}

	r.mu.Lock()

	for _, e := range entries {
		if _, ok := r.tables[e.Table]; !ok {
			r.mu.Unlock()

			return errors.Errorf("no table %q", e.Table)
		}

		byTable[e.Table] = append(byTable[e.Table], e.wireEntry)
	}

	tables := make(map[string]writer, len(byTable))
	for name := range byTable {
		tables[name] = r.tables[name]
	}

	r.mu.Unlock()

	return r.db.Batch(func(tx *bbolt.Tx) error {
		for name, es := range byTable {
			if err := tables[name].insertTx(tx, es); err != nil {
				return err
			}
		}

		return nil
	})
}

// Write merges rows, of any tables of one member, into their replicas, and
// returns once each row has a quorum in the replica set of every retained
// layout version. Every node gets its rows in one request and applies them in
// one transaction.
func Write(ctx context.Context, member *peer.Member, rows ...Row) error {
	if len(rows) == 0 {
		return nil
	}

	v, ok := registries.Load(member)
	if !ok {
		return errors.New("no tables on this member")
	}

	reg := v.(*registry) //nolint:forcetypeassert // Only registries are stored.

	// Every (row, replica set) needs its own quorum.
	type target struct {
		row   int
		nodes []layout.NodeID
		need  int
		ok    int
		fail  int
	}

	var targets []*target

	byNode := map[layout.NodeID][]tableEntry{}

	for i, r := range rows {
		sets, err := r.t.writeSets(r.entry.PK)
		if err != nil {
			return err
		}

		sent := map[layout.NodeID]bool{}

		for _, nodes := range sets {
			targets = append(targets, &target{row: i, nodes: nodes, need: len(nodes)/2 + 1})

			for _, id := range nodes {
				if !sent[id] {
					sent[id] = true
					byNode[id] = append(byNode[id], tableEntry{Table: r.t.tableName(), wireEntry: r.entry})
				}
			}
		}
	}

	// Nodes past the quorum still get the write: detach from the caller.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	type result struct {
		node layout.NodeID
		err  error
	}

	results := make(chan result, len(byNode))

	var wg sync.WaitGroup

	for id, entries := range byNode {
		wg.Go(func() {
			var err error
			if id == member.ID() {
				err = reg.apply(entries)
			} else {
				err = member.Call(bg, id, "POST", "/v1/tables/write", writeReq{Entries: entries}, nil)
			}

			results <- result{id, err}
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	var errs []error

	for range byNode {
		res := <-results
		if res.err != nil {
			errs = append(errs, res.err)
		}

		done := true

		for _, t := range targets {
			if slices.Contains(t.nodes, res.node) {
				if res.err != nil {
					t.fail++
				} else {
					t.ok++
				}
			}

			if len(t.nodes)-t.fail < t.need {
				return errors.Wrapf(ErrQuorum, "%d of %d replicas failed: %v", t.fail, len(t.nodes), errors.Join(errs...))
			}

			if t.ok < t.need {
				done = false
			}
		}

		if done {
			return nil
		}
	}

	return errors.Wrap(ErrQuorum, "not enough replicas")
}
