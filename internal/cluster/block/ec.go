package block

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"github.com/klauspost/reedsolomon"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Erasure coding: a block can be stored as K data and M parity shards instead
// of Replicas whole copies — ec:4,2 holds 1.5× its size and survives any two
// lost shards, where three replicas hold 3× and survive two lost copies.
//
// The code is systematic: the data shards are the block's own bytes, cut in
// K, so a healthy read fetches them and decodes nothing. Shard i lives on slot
// i of the partition the block's hash maps to, so it is found from the hash
// alone, like a replica, and the layout spreads the first K+M slots over zones
// and racks as it does the first three.

// replicated is the name of the default scheme: Replicas whole copies.
const replicated = "rf3"

// Scheme is how a block is stored: replicated (the zero value) or erasure
// coded as K data and M parity shards.
type Scheme struct {
	K, M int
}

// Coded reports whether the scheme is erasure coded.
func (s Scheme) Coded() bool { return s.K > 0 }

func (s Scheme) String() string {
	if !s.Coded() {
		return replicated
	}

	return "ec:" + strconv.Itoa(s.K) + "," + strconv.Itoa(s.M)
}

// ParseScheme parses "rf3", or "ec:K,M" with 2 ≤ K, 1 ≤ M and K+M at most
// 16: wider codes cost more to repair than they save.
func ParseScheme(v string) (Scheme, error) {
	if v == replicated || v == "" {
		return Scheme{}, nil
	}

	k, m, ok := strings.Cut(strings.TrimPrefix(v, "ec:"), ",")
	if !ok || !strings.HasPrefix(v, "ec:") {
		return Scheme{}, errors.Errorf("bad scheme %q: want rf3 or ec:K,M", v)
	}

	s := Scheme{}

	var err1, err2 error

	s.K, err1 = strconv.Atoi(k)
	s.M, err2 = strconv.Atoi(m)

	if err1 != nil || err2 != nil || s.K < 2 || s.M < 1 || s.K+s.M > 16 {
		return Scheme{}, errors.Errorf("bad scheme %q: want ec:K,M with K ≥ 2, M ≥ 1, K+M ≤ 16", v)
	}

	return s, nil
}

//nolint:gochecknoglobals // Codecs are immutable and costly to build.
var codecs sync.Map // Scheme → reedsolomon.Encoder

func codec(s Scheme) (reedsolomon.Encoder, error) {
	if c, ok := codecs.Load(s); ok {
		return c.(reedsolomon.Encoder), nil //nolint:forcetypeassert // Only encoders are stored.
	}

	c, err := reedsolomon.New(s.K, s.M)
	if err != nil {
		return nil, errors.Wrapf(err, "codec %s", s)
	}

	codecs.Store(s, c)

	return c, nil
}

// slotsFor returns the nodes holding h's shards under s, in shard order.
func (m *Manager) slotsFor(h Hash, s Scheme) ([]layout.NodeID, error) {
	l := m.member.Layout()
	if l == nil {
		return nil, ErrNoLayout
	}

	slots := l.Slots[l.Partition(h[:])]
	if len(slots) < s.K+s.M {
		return nil, errors.Errorf("%s needs %d nodes per partition; the layout has %d", s, s.K+s.M, len(slots))
	}

	return slots[:s.K+s.M], nil
}

// PutCoded stores data, which hashes to h, as the shards of s on their
// nodes, and returns once K+1 hold theirs: one more than a read needs, so a
// write acknowledged survives losing any one of them. The rest are still
// written; a node that misses its shard has it rebuilt by repair.
//
// release, when not nil, is called once nothing reads data any more.
func (m *Manager) PutCoded(ctx context.Context, h Hash, data []byte, s Scheme, release func()) error {
	var once sync.Once

	done := func() {
		if release != nil {
			once.Do(release)
		}
	}

	nodes, err := m.slotsFor(h, s)
	if err != nil {
		done()

		return err
	}

	enc, err := codec(s)
	if err != nil {
		done()

		return err
	}

	// A full slice: Split pads and places parity in spare capacity, which is
	// the caller's.
	shards, err := enc.Split(data[:len(data):len(data)])
	if err != nil {
		done()

		return errors.Wrap(err, "split block")
	}

	if err := enc.Encode(shards); err != nil {
		done()

		return errors.Wrap(err, "encode block")
	}

	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundTimeout)
	results := make(chan error, len(nodes))

	var wg sync.WaitGroup

	for i, id := range nodes {
		wg.Go(func() {
			results <- m.putShardOn(bg, id, Shard{Hash: h, K: s.K, M: s.M, I: i}, shards[i])
		})
	}

	go func() {
		wg.Wait()
		cancel()
		done()
	}()

	need := min(s.K+1, len(nodes))

	var (
		ok   int
		errs []error
	)

	for range nodes {
		if err := <-results; err != nil {
			errs = append(errs, err)
			if len(nodes)-len(errs) < need {
				return errors.Wrapf(ErrQuorum, "block %s (%s): %v", h, s, errors.Join(errs...))
			}

			continue
		}

		ok++
		if ok == need {
			return nil
		}
	}

	return errors.Wrapf(ErrQuorum, "block %s (%s)", h, s)
}

// GetCoded returns the size-byte block h stored under s: its data shards
// joined, or, when any is missing or corrupt, the block rebuilt from any K
// shards. Fewer than K reachable is ErrNotFound.
func (m *Manager) GetCoded(ctx context.Context, h Hash, size int, s Scheme) ([]byte, error) {
	nodes, err := m.slotsFor(h, s)
	if err != nil {
		return nil, err
	}

	enc, err := codec(s)
	if err != nil {
		return nil, err
	}

	shards := make([][]byte, len(nodes))
	fetch := func(idx []int) {
		var wg sync.WaitGroup

		for _, i := range idx {
			wg.Go(func() {
				data, err := m.getShardFrom(ctx, nodes[i], Shard{Hash: h, K: s.K, M: s.M, I: i})
				if err == nil {
					shards[i] = data
				}
			})
		}

		wg.Wait()
	}

	data := make([]int, s.K)
	for i := range data {
		data[i] = i
	}

	fetch(data)

	have := 0

	for _, sh := range shards[:s.K] {
		if sh != nil {
			have++
		}
	}

	if have < s.K {
		m.degraded.Add(1)

		// As many parity shards as data shards are missing, then the rest
		// if some of those are missing too.
		parity := make([]int, 0, s.M)
		for i := s.K; i < len(nodes); i++ {
			parity = append(parity, i)
		}

		first := min(s.K-have, len(parity))
		fetch(parity[:first])

		for _, sh := range shards[s.K:] {
			if sh != nil {
				have++
			}
		}

		if have < s.K {
			fetch(parity[first:])
		}

		if err := enc.ReconstructData(shards); err != nil {
			return nil, errors.Wrapf(ErrNotFound, "block %s (%s): %v", h, s, err)
		}
	}

	out := make([]byte, 0, size)
	for _, sh := range shards[:s.K] {
		out = append(out, sh...)
	}

	if len(out) < size {
		return nil, errors.Wrapf(ErrCorrupt, "block %s (%s): shards hold %d of %d bytes", h, s, len(out), size)
	}

	return out[:size], nil
}

// gcShards removes this node's shards of blocks live reports unreferenced,
// as GC does whole blocks.
func (m *Manager) gcShards(ctx context.Context, cutoff time.Time, live func(context.Context, Hash) (bool, error)) (int, error) {
	removed := 0

	err := m.store.WalkShards(func(sh Shard, mod time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if mod.After(cutoff) {
			return nil
		}

		ok, err := live(ctx, sh.Hash)
		if err != nil || ok {
			return nil //nolint:nilerr // A reference we could not check is one we keep.
		}

		deleted, err := m.store.deleteIfOlder(sh.String(), cutoff)
		if err != nil {
			return err
		}

		if !deleted {
			return nil
		}

		removed++

		m.collected.Add(1)

		return nil
	})

	return removed, err
}

func (m *Manager) putShardOn(ctx context.Context, id layout.NodeID, sh Shard, data []byte) error {
	if id == m.member.ID() {
		return m.store.PutShard(sh, data)
	}

	status, _, err := m.member.Raw(ctx, id, http.MethodPut, "/v1/shard/"+sh.String(), data)
	if err != nil {
		return err
	}

	if status != http.StatusNoContent {
		return errors.Errorf("put shard %s on %s: status %d", sh, id, status)
	}

	return nil
}

func (m *Manager) getShardFrom(ctx context.Context, id layout.NodeID, sh Shard) ([]byte, error) {
	if id == m.member.ID() {
		data, err := m.store.GetShard(sh)
		if errors.Is(err, ErrCorrupt) {
			m.corrupt.Add(1)
		}

		return data, err
	}

	// The transport is authenticated end to end, so what arrives is what
	// the peer read, and the peer checked it against its CRC.
	status, data, err := m.member.Raw(ctx, id, http.MethodGet, "/v1/shard/"+sh.String(), nil)

	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNotFound:
		return nil, ErrNotFound
	case status != http.StatusOK:
		return nil, errors.Errorf("get shard %s from %s: status %d", sh, id, status)
	}

	return data, nil
}

func (m *Manager) servePutShard(w http.ResponseWriter, r *http.Request) {
	sh, err := ParseShard(r.PathValue("shard"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	data, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if err := m.store.PutShard(sh, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) serveGetShard(w http.ResponseWriter, r *http.Request) {
	sh, err := ParseShard(r.PathValue("shard"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	data, err := m.store.GetShard(sh)

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
		_, _ = w.Write(data) //nolint:gosec // Shard bytes as octet-stream to an authenticated peer.
	}
}
