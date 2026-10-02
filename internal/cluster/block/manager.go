package block

import (
	"context"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
)

// Replicas is how many nodes hold each block, when the layout has that many
// slots per partition.
const Replicas = 3

// MaxSize bounds a block. The engine cuts objects into blocks well below it.
const MaxSize = 32 << 20

// DefaultGrace is how long an unreferenced block is kept: long enough for an
// upload to write its blocks and then the references to them.
const DefaultGrace = 10 * time.Minute

// backgroundTimeout bounds the writes a Put leaves running past its quorum.
const backgroundTimeout = 30 * time.Second

var (
	// ErrNoLayout is returned before the cluster has a layout.
	ErrNoLayout = errors.New("no cluster layout")
	// ErrQuorum is returned when too few replicas stored a block.
	ErrQuorum = errors.New("quorum not reached")
)

// Stats are a Manager's counters, for metrics.
type Stats struct {
	// ResyncPending is how many (block, replica) pairs wait to be copied —
	// blocks missing a healthy replica. Its growth is the number to watch.
	ResyncPending int64
	ResyncDone    int64
	ResyncFailed  int64
	// Corrupt counts copies found not matching their hash, here or on a peer.
	Corrupt int64
	// Collected counts blocks removed by GC.
	Collected int64
}

// Manager stores blocks across the cluster.
type Manager struct {
	store  *Store
	member *peer.Member

	mu     sync.Mutex
	resync map[resyncKey]resyncItem

	done, failed, corrupt, collected atomic.Int64
}

type resyncKey struct {
	hash Hash
	node layout.NodeID
}

type resyncItem struct {
	attempts int
	due      time.Time
}

// maxResyncAttempts bounds retries of one copy.
//
// ponytail: the queue is in memory and gives up after a few attempts; a
// restart or a long outage leaves the gap to anti-entropy (#275), which
// compares replicas instead of remembering what failed.
const maxResyncAttempts = 8

// NewManager returns a Manager over this node's store and registers the
// block endpoints on member; call it before serving.
func NewManager(store *Store, member *peer.Member) *Manager {
	m := &Manager{store: store, member: member, resync: map[resyncKey]resyncItem{}}

	member.Handle("PUT /v1/block/{hash}", http.HandlerFunc(m.servePut))
	member.Handle("GET /v1/block/{hash}", http.HandlerFunc(m.serveGet))

	return m
}

// Put stores data on the block's replicas and returns its hash once a quorum
// has it. Replicas that fail are queued for resync.
func (m *Manager) Put(ctx context.Context, data []byte) (Hash, error) {
	h := Sum(data)

	if len(data) > MaxSize {
		return h, errors.Errorf("block of %d bytes exceeds %d", len(data), MaxSize)
	}

	nodes, err := m.replicas(h)
	if err != nil {
		return h, err
	}

	// Replicas past the quorum still get the block: detach from the caller,
	// who stops waiting once a quorum has it.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)

	results := make(chan error, len(nodes))

	var wg sync.WaitGroup

	for _, id := range nodes {
		wg.Go(func() {
			err := m.putOn(bg, id, h, data)
			if err != nil {
				m.enqueue(h, id)
			}

			results <- err
		})
	}

	go func() {
		wg.Wait()
		cancel()
	}()

	need := len(nodes)/2 + 1

	var ok, failed int

	var errs []error

	for range nodes {
		if err := <-results; err != nil {
			failed++

			errs = append(errs, err)

			if len(nodes)-failed < need {
				return h, errors.Wrapf(ErrQuorum, "block %s: %v", h, errors.Join(errs...))
			}

			continue
		}

		ok++
		if ok == need {
			return h, nil
		}
	}

	return h, errors.Wrapf(ErrQuorum, "block %s", h)
}

// Get returns the block's content from the first replica that has a valid
// copy, trying this node first. Replicas found missing or corrupt on the way
// are queued for resync.
func (m *Manager) Get(ctx context.Context, h Hash) ([]byte, error) {
	nodes, err := m.replicas(h)
	if err != nil {
		return nil, err
	}

	// This node first: no network hop when it is a replica.
	slices.SortStableFunc(nodes, func(a, b layout.NodeID) int {
		switch {
		case a == m.member.ID():
			return -1
		case b == m.member.ID():
			return 1
		default:
			return 0
		}
	})

	var (
		missing []layout.NodeID
		errs    []error
	)

	for _, id := range nodes {
		data, err := m.getFrom(ctx, id, h)
		if err == nil {
			for _, n := range missing {
				m.enqueue(h, n)
			}

			return data, nil
		}

		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
			missing = append(missing, id)
		}

		errs = append(errs, err)
	}

	if len(missing) == len(nodes) {
		return nil, errors.Wrapf(ErrNotFound, "block %s on every replica", h)
	}

	return nil, errors.Wrapf(errors.Join(errs...), "block %s", h)
}

// GC removes this node's blocks that live reports unreferenced and that have
// not been written or touched within grace, and temporary files a crash left
// behind. It returns how many blocks it removed.
//
// ponytail: blocks of partitions the layout moved off this node are kept;
// handing them over and dropping them belongs to anti-entropy (#275).
func (m *Manager) GC(ctx context.Context, grace time.Duration, live func(context.Context, Hash) (bool, error)) (int, error) {
	cutoff := time.Now().Add(-grace)
	removed := 0

	err := m.store.Walk(func(h Hash, tmp bool, mod time.Time, path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if mod.After(cutoff) {
			return nil
		}

		if tmp {
			_ = os.Remove(path)

			return nil
		}

		ok, err := live(ctx, h)
		if err != nil {
			// A reference we could not check is a reference we keep.
			return nil //nolint:nilerr // Skip this block, keep collecting the rest.
		}

		if ok {
			return nil
		}

		if err := m.store.Delete(h); err != nil {
			return err
		}

		removed++

		m.collected.Add(1)

		return nil
	})

	return removed, err
}

// Resync copies every due queued block to the replica missing it, fetching it
// from any replica that has it.
func (m *Manager) Resync(ctx context.Context) {
	now := time.Now()

	m.mu.Lock()

	var due []resyncKey

	for k, it := range m.resync {
		if !it.due.After(now) {
			due = append(due, k)
		}
	}
	m.mu.Unlock()

	for _, k := range due {
		err := m.copyTo(ctx, k)

		m.mu.Lock()

		it := m.resync[k]

		switch {
		case err == nil:
			delete(m.resync, k)
			m.done.Add(1)
		case it.attempts+1 >= maxResyncAttempts:
			delete(m.resync, k)
			m.failed.Add(1)
		default:
			it.attempts++
			it.due = time.Now().Add(time.Second << it.attempts)
			m.resync[k] = it
		}

		m.mu.Unlock()
	}
}

// Run resyncs every interval until ctx is canceled.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Resync(ctx)
		}
	}
}

// Stats returns the counters.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	pending := int64(len(m.resync))
	m.mu.Unlock()

	return Stats{
		ResyncPending: pending,
		ResyncDone:    m.done.Load(),
		ResyncFailed:  m.failed.Load(),
		Corrupt:       m.corrupt.Load(),
		Collected:     m.collected.Load(),
	}
}

func (m *Manager) copyTo(ctx context.Context, k resyncKey) error {
	if k.node == m.member.ID() && m.store.Has(k.hash) {
		return nil
	}

	data, err := m.Get(ctx, k.hash)
	if err != nil {
		return err
	}

	return m.putOn(ctx, k.node, k.hash, data)
}

func (m *Manager) enqueue(h Hash, node layout.NodeID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := resyncKey{h, node}
	if _, ok := m.resync[k]; !ok {
		m.resync[k] = resyncItem{due: time.Now()}
	}
}

// replicas returns the nodes holding h.
func (m *Manager) replicas(h Hash) ([]layout.NodeID, error) {
	l := m.member.Layout()
	if l == nil {
		return nil, ErrNoLayout
	}

	slots := l.Slots[l.Partition(h[:])]

	return slices.Clone(slots[:min(Replicas, len(slots))]), nil
}

func (m *Manager) putOn(ctx context.Context, id layout.NodeID, h Hash, data []byte) error {
	if id == m.member.ID() {
		return m.store.Put(h, data)
	}

	status, _, err := m.member.Raw(ctx, id, http.MethodPut, "/v1/block/"+h.String(), data)
	if err != nil {
		return err
	}

	if status != http.StatusNoContent {
		return errors.Errorf("put block %s on %s: status %d", h, id, status)
	}

	return nil
}

func (m *Manager) getFrom(ctx context.Context, id layout.NodeID, h Hash) ([]byte, error) {
	if id == m.member.ID() {
		data, err := m.store.Get(h)
		if errors.Is(err, ErrCorrupt) {
			m.corrupt.Add(1)
		}

		return data, err
	}

	status, data, err := m.member.Raw(ctx, id, http.MethodGet, "/v1/block/"+h.String(), nil)
	if err != nil {
		return nil, err
	}

	switch {
	case status == http.StatusNotFound:
		return nil, ErrNotFound
	case status != http.StatusOK:
		return nil, errors.Errorf("get block %s from %s: status %d", h, id, status)
	case Sum(data) != h:
		// The transport is authenticated, so this is the peer's disk.
		m.corrupt.Add(1)

		return nil, ErrCorrupt
	}

	return data, nil
}

func (m *Manager) servePut(w http.ResponseWriter, r *http.Request) {
	h, err := ParseHash(r.PathValue("hash"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	data, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	switch err := m.store.Put(h, data); {
	case errors.Is(err, ErrMismatch):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (m *Manager) serveGet(w http.ResponseWriter, r *http.Request) {
	h, err := ParseHash(r.PathValue("hash"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	data, err := m.store.Get(h)

	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrCorrupt):
		m.corrupt.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}
}

func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxSize+1))
	if err != nil {
		return nil, errors.Wrap(err, "read block")
	}

	if len(data) > MaxSize {
		return nil, errors.Errorf("block exceeds %d bytes", MaxSize)
	}

	return data, nil
}
