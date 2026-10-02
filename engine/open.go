package engine

import (
	"crypto/rand"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/peer"
	"github.com/go-faster/fs/internal/cluster/table"
	"github.com/go-faster/fs/internal/sse"
)

// Options configure Open.
type Options struct {
	// Keyring seals the data keys of encrypted objects; nil refuses
	// encryption.
	Keyring *sse.Keyring
	// NoSync skips fsync of metadata and blocks: an acknowledged write can be
	// lost in a crash. For tests and development only.
	NoSync bool
	// Member is the cluster membership the engine replicates over. The fs
	// binary sets it in cluster mode; nil runs a single node, with a private
	// one-node layout kept under dir.
	Member *peer.Member
}

// soloID is a single node's identity in its own one-node layout.
const soloID = "local"

// Open opens the engine stored under dir, creating it on first use.
func Open(dir string, opts Options) (*Engine, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, errors.Wrap(err, "create engine dir")
	}

	member := opts.Member
	if member == nil {
		var err error
		if member, err = soloMember(filepath.Join(dir, "solo")); err != nil {
			return nil, err
		}
	}

	db, err := table.OpenDB(filepath.Join(dir, "meta.db"))
	if err != nil {
		return nil, err
	}

	db.NoSync = opts.NoSync

	store, err := block.NewStore(filepath.Join(dir, "blocks"))
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	store.NoSync = opts.NoSync

	e, err := New(Config{Member: member, DB: db, Blocks: block.NewManager(store, member), Keyring: opts.Keyring})
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	return e, nil
}

// Close releases the metadata database.
func (e *Engine) Close() error {
	if err := e.db.Close(); err != nil {
		return errors.Wrap(err, "close metadata")
	}

	return nil
}

// soloMember is a single node's membership: never served, with a one-node
// layout applied the first time.
func soloMember(dir string) (*peer.Member, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, errors.Wrap(err, "solo secret")
	}

	m, err := peer.New(peer.Config{ID: soloID, Addr: soloID, Secret: secret, Dir: dir})
	if err != nil {
		return nil, errors.Wrap(err, "solo membership")
	}

	if m.Layout() == nil {
		roles := []layout.Node{{ID: soloID, Capacity: 1}}
		if _, _, err := m.Apply(roles, layout.Options{Partitions: 1, Widths: []int{1}}, false); err != nil {
			return nil, errors.Wrap(err, "solo layout")
		}
	}

	return m, nil
}
