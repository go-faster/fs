// Package table is the cluster's replicated metadata: tables of rows that
// every replica can merge without coordination.
//
// A row is addressed by a partition key and a sort key. The partition key
// picks the layout partition, and so the replicas: the first Replicas slots of
// that partition. Rows within one partition key are stored in sort-key order,
// so a range over them — a bucket's listing — is one scan on each replica.
//
// Rows are CRDTs: the table is given a merge function that must be
// commutative, associative and idempotent. Writes go to every replica and
// return once a quorum has merged the row; reads ask a quorum, merge what
// comes back and write the merged row to any replica that answered with
// something older. Replicas therefore never need to agree on an order of
// writes, and none is special.
package table

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
)

// Replicas is how many nodes hold each row, when the layout has that many
// slots per partition.
const Replicas = 3

// backgroundTimeout bounds the work a call leaves running after it returns:
// writes to replicas beyond the quorum, and read repair.
const backgroundTimeout = 30 * time.Second

// ErrNoLayout is returned before the cluster has a layout.
var ErrNoLayout = errors.New("no cluster layout")

// ErrQuorum is returned when too few replicas answered.
var ErrQuorum = errors.New("quorum not reached")

// Entry is one row with its keys.
type Entry[R any] struct {
	PK, SK string
	Row    R
}

// Table is a replicated table of rows of type R.
type Table[R any] struct {
	name   string
	merge  func(a, b R) R
	db     *bbolt.DB
	member *peer.Member
	sync   syncState
}

// New returns the table name stored in db and replicated through member.
// merge must be commutative, associative and idempotent: replicas apply
// writes in any order, any number of times. New registers the table's peer
// endpoints on member, so call it before serving.
func New[R any](name string, db *bbolt.DB, member *peer.Member, merge func(a, b R) R) (*Table[R], error) {
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(name))

		return err
	}); err != nil {
		return nil, errors.Wrapf(err, "create table %q", name)
	}

	t := &Table[R]{name: name, merge: merge, db: db, member: member}

	member.Handle("POST /v1/table/"+name+"/insert", handle(t.serveInsert))
	member.Handle("POST /v1/table/"+name+"/get", handle(t.serveGet))
	member.Handle("POST /v1/table/"+name+"/range", handle(t.serveRange))
	t.registerSync()

	return t, nil
}

// Insert merges row into the row at (pk, sk) on every replica, and returns
// once a quorum has it.
func (t *Table[R]) Insert(ctx context.Context, pk, sk string, row R) error {
	b, err := json.Marshal(row)
	if err != nil {
		return errors.Wrap(err, "encode row")
	}

	nodes, err := t.replicas(pk)
	if err != nil {
		return err
	}

	req := insertReq{Entries: []wireEntry{{PK: pk, SK: sk, Row: b}}}

	// Replicas past the quorum still get the write: detach from the caller,
	// who stops waiting once a quorum has it, and release the context only
	// when every replica has answered.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	var wg sync.WaitGroup

	wg.Add(len(nodes))

	go func() {
		wg.Wait()
		cancel()
	}()

	_, err = quorum(nodes, len(nodes)/2+1, func(id layout.NodeID) (struct{}, error) {
		defer wg.Done()

		return struct{}{}, t.insertOn(ctx, id, req)
	})

	return err
}

// Get returns the merged row at (pk, sk) as a quorum of replicas holds it.
func (t *Table[R]) Get(ctx context.Context, pk, sk string) (row R, found bool, err error) {
	nodes, err := t.replicas(pk)
	if err != nil {
		return row, false, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type answer struct {
		node layout.NodeID
		resp getResp
	}

	answers, err := quorum(nodes, len(nodes)/2+1, func(id layout.NodeID) (answer, error) {
		var resp getResp

		err := t.on(ctx, id, "get", getReq{PK: pk, SK: sk}, &resp, func() (any, error) {
			return t.localGet(pk, sk)
		})

		return answer{id, resp}, err
	})
	if err != nil {
		return row, false, err
	}

	for _, a := range answers {
		if !a.resp.Found {
			continue
		}

		got, err := t.decode(a.resp.Row)
		if err != nil {
			return row, false, err
		}

		if found {
			row = t.merge(row, got)
		} else {
			row, found = got, true
		}
	}

	if !found {
		return row, false, nil
	}

	enc, err := json.Marshal(row)
	if err != nil {
		return row, false, errors.Wrap(err, "encode row")
	}

	var stale []layout.NodeID

	for _, a := range answers {
		if !bytes.Equal(a.resp.Row, enc) {
			stale = append(stale, a.node)
		}
	}

	t.repair(ctx, stale, []wireEntry{{PK: pk, SK: sk, Row: enc}})

	return row, true, nil
}

// Range returns up to limit rows of partition key pk with a sort key at or
// after start, in sort-key order, merged across a quorum of replicas.
func (t *Table[R]) Range(ctx context.Context, pk, start string, limit int) ([]Entry[R], error) {
	if limit <= 0 {
		return nil, nil
	}

	nodes, err := t.replicas(pk)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type answer struct {
		node    layout.NodeID
		entries []wireEntry
	}

	answers, err := quorum(nodes, len(nodes)/2+1, func(id layout.NodeID) (answer, error) {
		var resp rangeResp

		err := t.on(ctx, id, "range", rangeReq{PK: pk, Start: start, Limit: limit}, &resp, func() (any, error) {
			return t.localRange(pk, start, limit)
		})

		return answer{id, resp.Entries}, err
	})
	if err != nil {
		return nil, err
	}

	merged := map[string]R{}

	for _, a := range answers {
		for _, e := range a.entries {
			row, err := t.decode(e.Row)
			if err != nil {
				return nil, err
			}

			if prev, ok := merged[e.SK]; ok {
				row = t.merge(prev, row)
			}

			merged[e.SK] = row
		}
	}

	keys := make([]string, 0, len(merged))
	for sk := range merged {
		keys = append(keys, sk)
	}

	sort.Strings(keys)
	keys = keys[:min(limit, len(keys))]

	out := make([]Entry[R], 0, len(keys))
	repairs := map[layout.NodeID][]wireEntry{}

	for _, sk := range keys {
		row := merged[sk]
		out = append(out, Entry[R]{PK: pk, SK: sk, Row: row})

		enc, err := json.Marshal(row)
		if err != nil {
			return nil, errors.Wrap(err, "encode row")
		}

		for _, a := range answers {
			// A replica that filled its limit before reaching sk said nothing
			// about it: not stale, just not asked.
			if len(a.entries) == limit && a.entries[len(a.entries)-1].SK < sk {
				continue
			}

			i := sort.Search(len(a.entries), func(i int) bool { return a.entries[i].SK >= sk })
			if i == len(a.entries) || a.entries[i].SK != sk || !bytes.Equal(a.entries[i].Row, enc) {
				repairs[a.node] = append(repairs[a.node], wireEntry{PK: pk, SK: sk, Row: enc})
			}
		}
	}

	for node, entries := range repairs {
		t.repair(ctx, []layout.NodeID{node}, entries)
	}

	return out, nil
}

// repair writes merged entries to replicas that answered with less, in the
// background.
func (t *Table[R]) repair(ctx context.Context, nodes []layout.NodeID, entries []wireEntry) {
	if len(nodes) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	go func() {
		defer cancel()

		for _, id := range nodes {
			// ponytail: a failed repair is dropped; anti-entropy (#275) is
			// what guarantees convergence, this only speeds it up.
			_ = t.insertOn(ctx, id, insertReq{Entries: entries})
		}
	}()
}

// replicas returns the nodes holding partition key pk.
func (t *Table[R]) replicas(pk string) ([]layout.NodeID, error) {
	l := t.member.Layout()
	if l == nil {
		return nil, ErrNoLayout
	}

	slots := l.Slots[l.Partition([]byte(pk))]

	return slots[:min(Replicas, len(slots))], nil
}

func (t *Table[R]) insertOn(ctx context.Context, id layout.NodeID, req insertReq) error {
	return t.on(ctx, id, "insert", req, nil, func() (any, error) {
		return nil, t.localInsert(req.Entries)
	})
}

// on runs op on node id: directly when it is this node, else over the peer
// protocol.
func (t *Table[R]) on(ctx context.Context, id layout.NodeID, op string, req, resp any, local func() (any, error)) error {
	if id != t.member.ID() {
		return t.member.Call(ctx, id, http.MethodPost, "/v1/table/"+t.name+"/"+op, req, resp)
	}

	out, err := local()
	if err != nil || resp == nil {
		return err
	}

	// The same shape a remote answer has, so callers need not care.
	b, err := json.Marshal(out)
	if err != nil {
		return errors.Wrap(err, "encode")
	}

	if err := json.Unmarshal(b, resp); err != nil {
		return errors.Wrap(err, "decode")
	}

	return nil
}

func (t *Table[R]) decode(b []byte) (R, error) {
	var row R
	if err := json.Unmarshal(b, &row); err != nil {
		return row, errors.Wrapf(err, "decode %s row", t.name)
	}

	return row, nil
}

// key orders rows by partition key, then sort key. The length prefix keeps one
// partition key from being a prefix of another's range.
func key(pk, sk string) []byte {
	b := binary.AppendUvarint(nil, uint64(len(pk)))
	b = append(b, pk...)

	return append(b, sk...)
}

func (t *Table[R]) localInsert(entries []wireEntry) error {
	// Batch coalesces concurrent inserts into one transaction, and may run fn
	// more than once — harmless, merging is idempotent.
	return t.db.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(t.name))

		for _, e := range entries {
			k := key(e.PK, e.SK)
			v := e.Row

			if old := b.Get(k); old != nil {
				prev, err := t.decode(old)
				if err != nil {
					return err
				}

				row, err := t.decode(e.Row)
				if err != nil {
					return err
				}

				if v, err = json.Marshal(t.merge(prev, row)); err != nil {
					return errors.Wrap(err, "encode row")
				}

				if bytes.Equal(v, old) {
					continue
				}
			}

			if err := b.Put(k, v); err != nil {
				return errors.Wrap(err, "put")
			}
		}

		return nil
	})
}

func (t *Table[R]) localGet(pk, sk string) (getResp, error) {
	var resp getResp

	err := t.db.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket([]byte(t.name)).Get(key(pk, sk)); v != nil {
			resp = getResp{Found: true, Row: bytes.Clone(v)}
		}

		return nil
	})

	return resp, err
}

func (t *Table[R]) localRange(pk, start string, limit int) (rangeResp, error) {
	var resp rangeResp

	prefix := key(pk, "")

	err := t.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket([]byte(t.name)).Cursor()

		for k, v := c.Seek(key(pk, start)); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if len(resp.Entries) == limit {
				break
			}

			resp.Entries = append(resp.Entries, wireEntry{PK: pk, SK: string(k[len(prefix):]), Row: bytes.Clone(v)})
		}

		return nil
	})

	return resp, err
}

// Wire shapes of the peer endpoints. Rows travel as their stored JSON, so a
// replica merges without the coordinator re-encoding anything.
type (
	wireEntry struct {
		PK  string          `json:"pk"`
		SK  string          `json:"sk"`
		Row json.RawMessage `json:"row"`
	}
	insertReq struct {
		Entries []wireEntry `json:"entries"`
	}
	getReq struct {
		PK string `json:"pk"`
		SK string `json:"sk"`
	}
	getResp struct {
		Found bool            `json:"found"`
		Row   json.RawMessage `json:"row,omitempty"`
	}
	rangeReq struct {
		PK    string `json:"pk"`
		Start string `json:"start"`
		Limit int    `json:"limit"`
	}
	rangeResp struct {
		Entries []wireEntry `json:"entries"`
	}
)

func (t *Table[R]) serveInsert(req insertReq) (any, error) { return nil, t.localInsert(req.Entries) }
func (t *Table[R]) serveGet(req getReq) (any, error)       { return t.localGet(req.PK, req.SK) }

func (t *Table[R]) serveRange(req rangeReq) (any, error) {
	return t.localRange(req.PK, req.Start, req.Limit)
}

// handle adapts a JSON request/response function to an http.Handler.
func handle[Req any](fn func(Req) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		resp, err := fn(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		if resp == nil {
			w.WriteHeader(http.StatusNoContent)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// quorum runs fn on every node in parallel and returns the first need
// successful results, or ErrQuorum as soon as need can no longer be reached.
// It does not wait for the rest: the channel holds every result, so a late fn
// finishes without blocking.
func quorum[T any](nodes []layout.NodeID, need int, fn func(layout.NodeID) (T, error)) ([]T, error) {
	type result struct {
		v   T
		err error
	}

	results := make(chan result, len(nodes))

	for _, id := range nodes {
		go func() {
			v, err := fn(id)
			results <- result{v, err}
		}()
	}

	var (
		ok   []T
		errs []error
	)

	for range nodes {
		r := <-results
		if r.err != nil {
			errs = append(errs, r.err)
			if len(nodes)-len(errs) < need {
				return nil, errors.Wrapf(ErrQuorum, "%d of %d replicas failed: %v", len(errs), len(nodes), errors.Join(errs...))
			}

			continue
		}

		ok = append(ok, r.v)
		if len(ok) == need {
			return ok, nil
		}
	}

	return nil, errors.Wrap(ErrQuorum, "not enough replicas")
}

// OpenDB opens the bbolt database tables live in, tuned for them.
//
// Writes go through bbolt's Batch, which holds each one up to MaxBatchDelay to
// coalesce it with concurrent writes. bbolt's default of 10ms is paid by a
// lone writer on every insert — several per object written — so it is cut to
// a millisecond: still enough to coalesce under load.
func OpenDB(path string) (*bbolt.DB, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, errors.Wrapf(err, "open %s", path)
	}

	db.MaxBatchDelay = time.Millisecond

	return db, nil
}
