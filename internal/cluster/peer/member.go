// Package peer is how cluster nodes find each other and agree on the layout.
//
// There is no coordinator. Every node keeps the layout it last adopted on
// disk and gossips with its peers: on each round it asks every peer it knows
// for its status, learns the peers they know, pulls a newer layout and pushes
// its own to a peer that is behind. The highest layout version wins, so an
// applied layout reaches every node within a few rounds.
//
// A layout change is a transition, not a switch. Until every node has synced
// the new layout — pulled what it now replicates and handed over what it no
// longer does — the versions before it stay retained: writes go to the
// replicas of every retained version and reads come from the oldest, which
// holds everything acknowledged. Each node gossips the newest version it has
// synced; an old version is retired once every node holding data in a
// retained version has synced past it. A node that will never sync again is
// released with Skip.
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
	"io"
	"maps"
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
	// Synced is the newest layout version this node has synced, and
	// SyncedBy what it knows of every node's, its own included.
	Synced   uint64                   `json:"synced,omitempty"`
	SyncedBy map[layout.NodeID]uint64 `json:"synced_by,omitempty"`
}

// PeerState is what a node knows about one of its peers.
type PeerState struct {
	Addr string
	// ID and Version are as of the last successful exchange.
	ID      layout.NodeID
	Version uint64
	// Synced is the newest layout version the peer has synced.
	Synced uint64
	// Seen is the time of the last successful exchange; zero if none.
	Seen time.Time
	// Err is the last exchange's failure, nil if it succeeded.
	Err error
}

// Member is one node's view of the cluster.
type Member struct {
	cfg    Config
	client *http.Client
	mux    *http.ServeMux

	applyMu sync.Mutex

	mu     sync.Mutex
	layout *layout.Layout
	digest string
	// history are the retained versions before layout, oldest first.
	history []*layout.Layout
	// synced is the newest version each node has reported synced.
	synced map[layout.NodeID]uint64
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
		peers:  map[string]*PeerState{},
		synced: map[layout.NodeID]uint64{},
		mux:    http.NewServeMux(),
	}

	m.routes()

	for _, addr := range cfg.Peers {
		m.learn(addr)
	}

	s, err := load(filepath.Join(cfg.Dir, layoutFile))
	if err != nil {
		return nil, err
	}

	if s != nil {
		m.layout, m.digest, m.history = s.Layout, digest(s.Layout), s.History
		if s.Synced != nil {
			m.synced = s.Synced
		}
	}

	return m, nil
}

// Layout returns the adopted layout, nil before the first one.
func (m *Member) Layout() *layout.Layout {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.layout
}

// Layouts returns the retained versions, oldest first, ending with the
// current one; empty before the first layout. Write to every one's replicas;
// read from the first's.
func (m *Member) Layouts() []*layout.Layout {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.layout == nil {
		return nil
	}

	return append(slices.Clone(m.history), m.layout)
}

// MarkSynced records that this node has synced layout version v: it holds
// what v gives it and has handed over what v moved away.
func (m *Member) MarkSynced(v uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if v <= m.synced[m.cfg.ID] {
		return nil
	}

	m.synced[m.cfg.ID] = v
	m.prune()

	return m.persist()
}

// Skip records node as synced through the current version, so retained
// versions can retire without it. For a node that is gone for good: data only
// it held is lost to the cluster's view.
func (m *Member) Skip(node layout.NodeID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.layout == nil {
		return errors.New("no layout")
	}

	m.synced[node] = max(m.synced[node], m.layout.Version)
	m.prune()

	return m.persist()
}

// Synced returns the newest version each node has reported synced.
func (m *Member) Synced() map[layout.NodeID]uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return maps.Clone(m.synced)
}

// prune retires the retained versions every data-holding node has synced
// past. Called with mu held.
func (m *Member) prune() {
	if len(m.history) == 0 {
		return
	}

	// The nodes that hold slots in any retained version.
	holders := map[layout.NodeID]bool{}

	for _, l := range append(slices.Clone(m.history), m.layout) {
		for _, slots := range l.Slots {
			for _, id := range slots {
				holders[id] = true
			}
		}
	}

	low := m.layout.Version
	for id := range holders {
		low = min(low, m.synced[id])
	}

	m.history = slices.DeleteFunc(m.history, func(l *layout.Layout) bool { return l.Version < low })
}

// persist saves the layout, history and sync state. Called with mu held.
func (m *Member) persist() error {
	return save(filepath.Join(m.cfg.Dir, layoutFile), &stored{
		Format: format, Layout: m.layout, History: m.history, Synced: m.synced,
	})
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

	return m.adopt(l, d, nil)
}

// adopt makes l current if it is newer, retaining the version it replaces
// and any of history, a peer's retained versions, that are newer than what
// this node has retired. Called with mu held.
func (m *Member) adopt(l *layout.Layout, d string, history []*layout.Layout) (bool, error) {
	cur := m.layout
	if cur != nil {
		if len(cur.Slots) != len(l.Slots) {
			return false, errors.Errorf("layout has %d partitions, this cluster has %d", len(l.Slots), len(cur.Slots))
		}

		if !newer(l.Version, d, cur.Version, m.digest) {
			return false, nil
		}
	}

	retained := slices.Clone(m.history)
	if cur != nil && cur.Version < l.Version {
		retained = append(retained, cur)
	}

	for _, h := range history {
		if h.Version < l.Version && !slices.ContainsFunc(retained, func(r *layout.Layout) bool { return r.Version == h.Version }) &&
			h.Validate() == nil && len(h.Slots) == len(l.Slots) {
			retained = append(retained, h)
		}
	}

	slices.SortFunc(retained, func(a, b *layout.Layout) int { return cmp.Compare(a.Version, b.Version) })

	prev := m.layout
	prevDigest := m.digest
	prevHistory := m.history

	m.layout, m.digest, m.history = l, d, retained
	m.prune()

	if err := m.persist(); err != nil {
		m.layout, m.digest, m.history = prev, prevDigest, prevHistory

		return false, err
	}

	return true, nil
}

// Apply computes the layout that follows the adopted one for nodes — the
// full set of member roles — and, unless dryRun, adopts it; gossip carries it
// from here. It reports the computed layout and how many slots it moves.
// Widths default to the adopted layout's.
func (m *Member) Apply(nodes []layout.Node, opts layout.Options, dryRun bool) (*layout.Layout, int, error) {
	// One apply at a time: two computed from the same layout would share a
	// version, and only one would survive.
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	cur := m.Layout()
	if cur != nil && len(opts.Widths) == 0 {
		opts.Widths = cur.Widths
	}

	next, err := layout.Compute(cur, nodes, opts)
	if err != nil {
		return nil, 0, err
	}

	moved := 0
	if cur != nil {
		moved = layout.Moved(cur, next)
	}

	if dryRun {
		return next, moved, nil
	}

	if _, err := m.Adopt(next); err != nil {
		return nil, 0, err
	}

	return next, moved, nil
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
	p.ID, p.Version, p.Synced = st.ID, st.Version, st.Synced
	cur, curDigest := m.layout, m.digest

	// A peer's own report is the authority on it; others are merged by
	// taking the newest, so a node released with Skip stays released.
	changed := false

	for id, v := range st.SyncedBy {
		if v > m.synced[id] {
			m.synced[id], changed = v, true
		}
	}

	if st.Synced > m.synced[st.ID] {
		m.synced[st.ID], changed = st.Synced, true
	}

	if changed && m.layout != nil {
		m.prune()
		_ = m.persist()
	}
	m.mu.Unlock()

	for _, a := range st.Peers {
		m.learn(a)
	}

	switch {
	case st.Version > 0 && (cur == nil || newer(st.Version, st.Digest, cur.Version, curDigest)):
		var ls layouts
		if err := m.call(ctx, http.MethodGet, addr, "/v1/layouts", nil, &ls); err != nil {
			return err
		}

		if err := m.adoptAll(ls); err != nil {
			return errors.Wrapf(err, "adopt layout from %s", addr)
		}
	case cur != nil && newer(cur.Version, curDigest, st.Version, st.Digest):
		return m.call(ctx, http.MethodPost, addr, "/v1/layouts", m.retained(), nil)
	}

	return nil
}

// layouts is a node's retained versions and current layout, as gossip moves
// them: a node that is behind needs the versions still in transition too,
// to write to their replicas.
type layouts struct {
	Current *layout.Layout   `json:"current"`
	History []*layout.Layout `json:"history,omitempty"`
}

func (m *Member) retained() layouts {
	m.mu.Lock()
	defer m.mu.Unlock()

	return layouts{Current: m.layout, History: slices.Clone(m.history)}
}

func (m *Member) adoptAll(ls layouts) error {
	if ls.Current == nil {
		return errors.New("no layout")
	}

	if err := ls.Current.Validate(); err != nil {
		return errors.Wrap(err, "invalid layout")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	_, err := m.adopt(ls.Current, digest(ls.Current), ls.History)

	return err
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

// Status is what this node reports about itself to its peers.
func (m *Member) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := Status{
		ID: m.cfg.ID, Addr: m.cfg.Addr, Digest: m.digest,
		Synced: m.synced[m.cfg.ID], SyncedBy: maps.Clone(m.synced),
	}
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
	return m.cfg.Secret.Handler(m.mux, m.cfg.Now)
}

// Handle registers another peer endpoint, served authenticated like the rest.
// Register before serving.
func (m *Member) Handle(pattern string, h http.Handler) {
	m.mux.Handle(pattern, h)
}

// ID is this node's identity.
func (m *Member) ID() layout.NodeID { return m.cfg.ID }

// Addr returns the address of the node with the given ID, as last learned by
// gossip.
func (m *Member) Addr(id layout.NodeID) (string, bool) {
	if id == m.cfg.ID {
		return m.cfg.Addr, true
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range m.peers {
		if p.ID == id {
			return p.Addr, true
		}
	}

	return "", false
}

// Call sends an authenticated JSON request to the node with the given ID and
// decodes the JSON answer into out, when out is not nil.
func (m *Member) Call(ctx context.Context, id layout.NodeID, method, path string, in, out any) error {
	addr, ok := m.Addr(id)
	if !ok {
		return errors.Errorf("no address known for node %q", id)
	}

	return m.call(ctx, method, addr, path, in, out)
}

func (m *Member) routes() {
	mux := m.mux

	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		// The caller just proved it holds the secret, so it is a peer worth
		// gossiping with — this is how a node joining with a single bootstrap
		// address becomes known to the rest.
		if from := r.URL.Query().Get("from"); from != "" {
			m.learn(from)
		}

		writeJSON(w, m.Status())
	})
	mux.HandleFunc("GET /v1/layouts", func(w http.ResponseWriter, _ *http.Request) {
		ls := m.retained()
		if ls.Current == nil {
			http.Error(w, "no layout", http.StatusNotFound)

			return
		}

		writeJSON(w, ls)
	})
	mux.HandleFunc("POST /v1/layouts", func(w http.ResponseWriter, r *http.Request) {
		var ls layouts
		if err := json.NewDecoder(r.Body).Decode(&ls); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		if err := m.adoptAll(ls); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)

			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
}

func (m *Member) call(ctx context.Context, method, addr, path string, in, out any) error {
	var body []byte

	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return errors.Wrap(err, "encode")
		}
	}

	status, resp, err := m.raw(ctx, method, addr, path, body)
	if err != nil {
		return err
	}

	if status/100 != 2 {
		return errors.Errorf("%s %s: status %d", method, path, status)
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(resp, out); err != nil {
		return errors.Wrapf(err, "decode %s", path)
	}

	return nil
}

// Raw sends an authenticated request with a raw body to the node with the
// given ID and returns the status and body of its answer. Unlike Call, a
// status that is not 2xx is an answer, not an error: a peer saying 404 is
// telling the caller something.
func (m *Member) Raw(ctx context.Context, id layout.NodeID, method, path string, body []byte) (status int, resp []byte, err error) {
	addr, ok := m.Addr(id)
	if !ok {
		return 0, nil, errors.Errorf("no address known for node %q", id)
	}

	return m.raw(ctx, method, addr, path, body)
}

func (m *Member) raw(ctx context.Context, method, addr, path string, body []byte) (code int, payload []byte, err error) {
	// from carries this node's address, so the callee can gossip back.
	u := url.URL{Scheme: "http", Host: addr, Path: path, RawQuery: url.Values{"from": {m.cfg.Addr}}.Encode()}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.Wrap(err, "build request")
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return 0, nil, errors.Wrapf(err, "%s %s", method, path)
	}

	defer func() { _ = resp.Body.Close() }()

	// The signing transport has already buffered and verified the body.
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, errors.Wrapf(err, "read %s", path)
	}

	return resp.StatusCode, b, nil
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

// stored is the layout file's shape. History and Synced came later; a file
// without them is a node with nothing in transition.
type stored struct {
	Format  int                      `json:"format"`
	Layout  *layout.Layout           `json:"layout"`
	History []*layout.Layout         `json:"history,omitempty"`
	Synced  map[layout.NodeID]uint64 `json:"synced,omitempty"`
}

func load(path string) (*stored, error) {
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

	for _, l := range append(slices.Clone(s.History), s.Layout) {
		if err := l.Validate(); err != nil {
			return nil, errors.Wrapf(err, "%s", path)
		}
	}

	return &s, nil
}

// save writes the layout durably: a temporary file, synced, renamed over the
// old one, and the directory synced, so a crash leaves the old layout or the
// new one and never a torn file.
func save(path string, s *stored) error {
	b, err := json.Marshal(s)
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
