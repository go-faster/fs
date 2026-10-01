// Package peer is how cluster nodes find each other and agree on the layout.
//
// There is no coordinator. Every node keeps the layout it last adopted on
// disk and gossips with its peers: on each round it asks every peer it knows
// for its status, learns the peers they know, pulls a newer layout and pushes
// its own to a peer that is behind. The highest layout version wins, so an
// applied layout reaches every node within a few rounds.
//
// All peer traffic is authenticated with the shared cluster secret; see
// Secret.
package peer

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// DefaultInterval is how often a node gossips with its peers.
const DefaultInterval = 2 * time.Second

// layoutFile holds the adopted layout, under Config.Dir.
const layoutFile = "layout.json"

// format stamps the layout file. A file with another stamp is refused rather
// than read as this one.
const format = 1

// Config configures a Member.
type Config struct {
	// ID is this node's identity in the layout.
	ID layout.NodeID
	// Addr is the host:port peers reach this node at.
	Addr string
	// Peers are host:port addresses to start gossiping with. More are learned
	// from them.
	Peers []string
	// Secret authenticates peer traffic. Every node must hold the same one.
	Secret Secret
	// Dir is where the adopted layout is kept.
	Dir string
	// Interval is the gossip period; zero means DefaultInterval.
	Interval time.Duration
	// Transport carries peer requests; nil means http.DefaultTransport.
	Transport http.RoundTripper
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Status is what a node reports about itself to its peers.
type Status struct {
	ID      layout.NodeID `json:"id"`
	Addr    string        `json:"addr"`
	Version uint64        `json:"version"`
	Digest  string        `json:"digest,omitempty"`
	Peers   []string      `json:"peers,omitempty"`
}

// PeerState is what a node knows about one of its peers.
type PeerState struct {
	Addr string
	// ID and Version are as of the last successful exchange.
	ID      layout.NodeID
	Version uint64
	// Seen is the time of the last successful exchange; zero if none.
	Seen time.Time
	// Err is the last exchange's failure, nil if it succeeded.
	Err error
}

// Member is one node's view of the cluster.
type Member struct {
	cfg    Config
	client *http.Client

	mu     sync.Mutex
	layout *layout.Layout
	digest string
	peers  map[string]*PeerState // by address
}

// New returns a Member, loading the layout adopted before a restart.
func New(cfg Config) (*Member, error) {
	if cfg.ID == "" || cfg.Addr == "" {
		return nil, errors.New("peer: node id and address are required")
	}

	if len(cfg.Secret) < 16 {
		return nil, errors.New("peer: the cluster secret must be at least 16 bytes")
	}

	cfg.Interval = cmp.Or(cfg.Interval, DefaultInterval)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	base := cfg.Transport
	if base == nil {
		base = http.DefaultTransport
	}

	m := &Member{
		cfg: cfg,
		client: &http.Client{
			Transport: &transport{secret: cfg.Secret, node: cfg.ID, base: base, now: cfg.Now},
			Timeout:   10 * time.Second,
		},
		peers: map[string]*PeerState{},
	}

	for _, addr := range cfg.Peers {
		m.learn(addr)
	}

	l, err := load(filepath.Join(cfg.Dir, layoutFile))
	if err != nil {
		return nil, err
	}

	if l != nil {
		m.layout, m.digest = l, digest(l)
	}

	return m, nil
}

// Layout returns the adopted layout, nil before the first one.
func (m *Member) Layout() *layout.Layout {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.layout
}

// Peers returns what this node knows about its peers, by address.
func (m *Member) Peers() []PeerState {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]PeerState, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, *p)
	}

	slices.SortFunc(out, func(a, b PeerState) int { return cmp.Compare(a.Addr, b.Addr) })

	return out
}

// Adopt makes l this node's layout if it is newer, persisting it first. It
// reports whether l was adopted; an older or equal layout is not an error.
//
// Two layouts of one version — applied concurrently on two nodes — are
// ordered by digest, so every node converges on the same one. The other is
// lost: apply layouts from one place.
func (m *Member) Adopt(l *layout.Layout) (bool, error) {
	if err := l.Validate(); err != nil {
		return false, errors.Wrap(err, "invalid layout")
	}

	d := digest(l)

	m.mu.Lock()
	defer m.mu.Unlock()

	if cur := m.layout; cur != nil {
		if len(cur.Slots) != len(l.Slots) {
			return false, errors.Errorf("layout has %d partitions, this cluster has %d", len(l.Slots), len(cur.Slots))
		}

		if !newer(l.Version, d, cur.Version, m.digest) {
			return false, nil
		}
	}

	if err := save(filepath.Join(m.cfg.Dir, layoutFile), l); err != nil {
		return false, err
	}

	m.layout, m.digest = l, d

	return true, nil
}

func newer(v uint64, d string, than uint64, thanDigest string) bool {
	return v > than || v == than && d > thanDigest
}

// Run gossips with peers until ctx is canceled.
func (m *Member) Run(ctx context.Context) {
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()

	for {
		m.Round(ctx)

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Round runs one gossip round: an exchange with every known peer, in
// parallel.
func (m *Member) Round(ctx context.Context) {
	m.mu.Lock()
	addrs := make([]string, 0, len(m.peers))

	for addr := range m.peers {
		addrs = append(addrs, addr)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup

	for _, addr := range addrs {
		wg.Go(func() {
			err := m.exchange(ctx, addr)

			m.mu.Lock()
			defer m.mu.Unlock()

			p := m.peers[addr]
			p.Err = err

			if err == nil {
				p.Seen = m.cfg.Now()
			}
		})
	}

	wg.Wait()
}

// exchange syncs with one peer: learn its peers, then pull its layout if it
// is newer or push ours if it is behind.
func (m *Member) exchange(ctx context.Context, addr string) error {
	var st Status
	if err := m.call(ctx, http.MethodGet, addr, "/v1/status", nil, &st); err != nil {
		return err
	}

	m.mu.Lock()
	p := m.peers[addr]
	p.ID, p.Version = st.ID, st.Version
	cur, curDigest := m.layout, m.digest
	m.mu.Unlock()

	for _, a := range st.Peers {
		m.learn(a)
	}

	switch {
	case st.Version > 0 && (cur == nil || newer(st.Version, st.Digest, cur.Version, curDigest)):
		var l layout.Layout
		if err := m.call(ctx, http.MethodGet, addr, "/v1/layout", nil, &l); err != nil {
			return err
		}

		if _, err := m.Adopt(&l); err != nil {
			return errors.Wrapf(err, "adopt layout from %s", addr)
		}
	case cur != nil && newer(cur.Version, curDigest, st.Version, st.Digest):
		return m.call(ctx, http.MethodPost, addr, "/v1/layout", cur, nil)
	}

	return nil
}

// learn adds a peer address, ignoring this node's own.
func (m *Member) learn(addr string) {
	if addr == "" || addr == m.cfg.Addr {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.peers[addr]; !ok {
		m.peers[addr] = &PeerState{Addr: addr}
	}
}

func (m *Member) status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := Status{ID: m.cfg.ID, Addr: m.cfg.Addr, Digest: m.digest}
	if m.layout != nil {
		st.Version = m.layout.Version
	}

	for addr := range m.peers {
		st.Peers = append(st.Peers, addr)
	}

	slices.Sort(st.Peers)

	return st
}

// Handler serves this node's side of the peer protocol, authenticated with the
// cluster secret.
func (m *Member) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		// The caller just proved it holds the secret, so it is a peer worth
		// gossiping with — this is how a node joining with a single bootstrap
		// address becomes known to the rest.
		if from := r.URL.Query().Get("from"); from != "" {
			m.learn(from)
		}

		writeJSON(w, m.status())
	})
	mux.HandleFunc("GET /v1/layout", func(w http.ResponseWriter, _ *http.Request) {
		l := m.Layout()
		if l == nil {
			http.Error(w, "no layout", http.StatusNotFound)

			return
		}

		writeJSON(w, l)
	})
	mux.HandleFunc("POST /v1/layout", func(w http.ResponseWriter, r *http.Request) {
		var l layout.Layout
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		if _, err := m.Adopt(&l); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)

			return
		}

		w.WriteHeader(http.StatusNoContent)
	})

	return m.cfg.Secret.Handler(mux, m.cfg.Now)
}

func (m *Member) call(ctx context.Context, method, addr, path string, in, out any) error {
	var body []byte

	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return errors.Wrap(err, "encode")
		}
	}

	// from carries this node's address, so the callee can gossip back.
	u := url.URL{Scheme: "http", Host: addr, Path: path, RawQuery: url.Values{"from": {m.cfg.Addr}}.Encode()}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.Wrap(err, "build request")
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return errors.Wrapf(err, "%s %s", method, path)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return errors.Errorf("%s %s: status %d", method, path, resp.StatusCode)
	}

	if out == nil {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return errors.Wrapf(err, "decode %s", path)
	}

	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func digest(l *layout.Layout) string {
	b, _ := json.Marshal(l)
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:8])
}

// stored is the layout file's shape.
type stored struct {
	Format int            `json:"format"`
	Layout *layout.Layout `json:"layout"`
}

func load(path string) (*layout.Layout, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- path is under the configured data directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, errors.Wrap(err, "read layout")
	}

	var s stored
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, errors.Wrapf(err, "parse %s", path)
	}

	if s.Format != format {
		return nil, errors.Errorf("%s has format %d, this binary reads %d", path, s.Format, format)
	}

	if s.Layout == nil {
		return nil, errors.Errorf("%s holds no layout", path)
	}

	if err := s.Layout.Validate(); err != nil {
		return nil, errors.Wrapf(err, "%s", path)
	}

	return s.Layout, nil
}

// save writes the layout durably: a temporary file, synced, renamed over the
// old one, and the directory synced, so a crash leaves the old layout or the
// new one and never a torn file.
func save(path string, l *layout.Layout) error {
	b, err := json.Marshal(stored{Format: format, Layout: l})
	if err != nil {
		return errors.Wrap(err, "encode layout")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return errors.Wrap(err, "create layout dir")
	}

	f, err := os.CreateTemp(dir, layoutFile+".tmp*")
	if err != nil {
		return errors.Wrap(err, "create layout")
	}

	defer func() { _ = os.Remove(f.Name()) }()

	if _, err := f.Write(b); err != nil {
		_ = f.Close()

		return errors.Wrap(err, "write layout")
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return errors.Wrap(err, "sync layout")
	}

	if err := f.Close(); err != nil {
		return errors.Wrap(err, "close layout")
	}

	if err := os.Rename(f.Name(), path); err != nil {
		return errors.Wrap(err, "replace layout")
	}

	// Windows journals directory metadata and cannot sync a directory handle.
	if runtime.GOOS == "windows" {
		return nil
	}

	d, err := os.Open(dir) // #nosec G304 -- the configured data directory
	if err != nil {
		return errors.Wrap(err, "open layout dir")
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return errors.Wrap(err, "sync layout dir")
	}

	return nil
}
