package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/go-faster/errors"
)

// Source identifies where a credential came from.
type Source string

const (
	// SourceConfig is a credential defined in the static config/env (read-only
	// at runtime).
	SourceConfig Source = "config"
	// SourceManaged is a credential created at runtime through the admin API
	// (editable and deletable, kept by the Backend).
	SourceManaged Source = "managed"
)

// KeyInfo describes a credential without exposing its secret. It is what the
// admin API lists.
type KeyInfo struct {
	AccessKey string
	Grants    []Grant
	Source    Source
	CreatedAt time.Time
}

// managedKey is the stored form of a runtime-created credential (secret
// included).
type managedKey struct {
	AccessKey string    `json:"access_key"`
	SecretKey string    `json:"secret_key"`
	Grants    []Grant   `json:"grants"`
	CreatedAt time.Time `json:"created_at"`
}

// Backend keeps the credentials created at runtime, as opaque records by
// access key. The storage engine is one: it replicates them, so every node of
// a cluster accepts a key created through any of them.
type Backend interface {
	AccessKeys(ctx context.Context) (map[string]json.RawMessage, error)
	PutAccessKey(ctx context.Context, id string, record json.RawMessage) error
	DeleteAccessKey(ctx context.Context, id string) error
}

// Manager owns the live auth Store and adds runtime CRUD over credentials.
// Config/env credentials form a read-only base; runtime-created credentials
// live in the Backend and are merged over the base. A change made here applies
// at once; one made through another node arrives with the next Refresh. Every
// change rebuilds the Store snapshot atomically, so live requests see it
// immediately.
type Manager struct {
	store   *Store
	backend Backend

	mu         sync.Mutex
	base       []Key // config/env credentials (read-only)
	publicRead []string
	managed    map[string]managedKey
	now        func() time.Time
}

// NewManager builds a Manager from a base Config (config/env credentials) and
// the Backend that keeps runtime-created credentials; a nil Backend keeps them
// in memory only. It does not read the Backend — a cluster may not serve reads
// yet — so call Refresh to load what it holds.
func NewManager(base Config, backend Backend) (*Manager, error) {
	if err := base.Validate(); err != nil {
		return nil, err
	}

	m := &Manager{
		backend:    backend,
		base:       append([]Key(nil), base.Keys...),
		publicRead: append([]string(nil), base.PublicReadBuckets...),
		managed:    make(map[string]managedKey),
		now:        time.Now,
	}

	store, err := NewStore(m.config())
	if err != nil {
		return nil, err
	}

	m.store = store

	return m, nil
}

// Store returns the live auth Store to wire into the server.
func (m *Manager) Store() *Store { return m.store }

// config assembles the effective Config from base + managed credentials. The
// caller must hold m.mu (or be in construction).
func (m *Manager) config() Config {
	keys := append([]Key(nil), m.base...)
	for _, mk := range m.managed {
		// A config key wins over a runtime one of the same ID: the config is
		// what the operator controls on this node.
		if m.isBase(mk.AccessKey) {
			continue
		}

		keys = append(keys, Key{AccessKey: mk.AccessKey, SecretKey: mk.SecretKey, Grants: mk.Grants})
	}

	return Config{Keys: keys, PublicReadBuckets: m.publicRead}
}

// isBase reports whether an access key comes from the static config.
func (m *Manager) isBase(accessKey string) bool {
	for _, k := range m.base {
		if k.AccessKey == accessKey {
			return true
		}
	}

	return false
}

// List returns every credential (config + managed), secrets omitted, sorted by
// access key.
func (m *Manager) List() []KeyInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	infos := make([]KeyInfo, 0, len(m.base)+len(m.managed))

	for _, k := range m.base {
		infos = append(infos, KeyInfo{AccessKey: k.AccessKey, Grants: k.Grants, Source: SourceConfig})
	}

	for _, mk := range m.managed {
		if m.isBase(mk.AccessKey) {
			continue
		}

		infos = append(infos, KeyInfo{
			AccessKey: mk.AccessKey, Grants: mk.Grants, Source: SourceManaged, CreatedAt: mk.CreatedAt,
		})
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].AccessKey < infos[j].AccessKey })

	return infos
}

// CreateInput describes a credential to create. AccessKey and SecretKey are
// generated when empty.
type CreateInput struct {
	AccessKey string
	SecretKey string
	Grants    []Grant
}

// Created is a newly created credential, including the secret — returned only
// once, at creation.
type Created struct {
	AccessKey string
	SecretKey string
	Grants    []Grant
	CreatedAt time.Time
}

// ErrKeyExists reports an access key that already exists.
var ErrKeyExists = errors.New("access key already exists")

// ErrKeyNotFound reports an unknown managed access key.
var ErrKeyNotFound = errors.New("access key not found")

// ErrKeyImmutable reports an attempt to modify a config-defined credential.
var ErrKeyImmutable = errors.New("access key is defined in config and cannot be modified at runtime")

// Create adds a runtime credential, generating the access key and/or secret
// when not supplied, stores it in the Backend, and applies it to the live
// Store. The returned Created carries the secret (the only time it is
// exposed).
//
// Whether the ID is taken is judged by what this node has seen; two nodes
// creating the same ID at once both succeed, and the later write wins.
func (m *Manager) Create(ctx context.Context, in CreateInput) (*Created, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	access := in.AccessKey
	if access == "" {
		access = NewAccessKey()
	}

	secret := in.SecretKey
	if secret == "" {
		secret = NewSecretKey()
	}

	if _, ok := m.managed[access]; ok || m.isBase(access) {
		return nil, errors.Wrapf(ErrKeyExists, "access key %q", access)
	}

	mk := managedKey{
		AccessKey: access,
		SecretKey: secret,
		Grants:    append([]Grant(nil), in.Grants...),
		CreatedAt: m.now().UTC(),
	}

	if m.backend != nil {
		record, err := json.Marshal(mk) //nolint:gosec // The record is the credential; storing its secret is the point.
		if err != nil {
			return nil, errors.Wrap(err, "encode access key")
		}

		if err := m.backend.PutAccessKey(ctx, access, record); err != nil {
			return nil, errors.Wrap(err, "store access key")
		}
	}

	m.managed[access] = mk

	if err := m.store.Set(m.config()); err != nil {
		return nil, err
	}

	return &Created{AccessKey: access, SecretKey: secret, Grants: mk.Grants, CreatedAt: mk.CreatedAt}, nil
}

// Delete removes a runtime credential. Config credentials cannot be deleted.
func (m *Manager) Delete(ctx context.Context, accessKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.isBase(accessKey) {
		return errors.Wrapf(ErrKeyImmutable, "access key %q", accessKey)
	}

	if _, ok := m.managed[accessKey]; !ok {
		return errors.Wrapf(ErrKeyNotFound, "access key %q", accessKey)
	}

	if m.backend != nil {
		if err := m.backend.DeleteAccessKey(ctx, accessKey); err != nil {
			return errors.Wrap(err, "delete access key")
		}
	}

	delete(m.managed, accessKey)

	return m.store.Set(m.config())
}

// Refresh replaces the runtime credentials with what the Backend holds, so
// keys created or deleted through another node take effect here. On an error
// the credentials stay as they were.
func (m *Manager) Refresh(ctx context.Context) error {
	if m.backend == nil {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	records, err := m.backend.AccessKeys(ctx)
	if err != nil {
		return errors.Wrap(err, "refresh")
	}

	managed := make(map[string]managedKey, len(records))

	for id, record := range records {
		var mk managedKey
		if err := json.Unmarshal(record, &mk); err != nil || mk.SecretKey == "" {
			// One unreadable record must not cost every other key.
			continue
		}

		mk.AccessKey = id
		managed[id] = mk
	}

	prev := m.managed
	m.managed = managed

	if err := m.store.Set(m.config()); err != nil {
		m.managed = prev

		return err
	}

	return nil
}

// Reload re-reads the static base credentials (e.g. after a SIGHUP config
// reload) while keeping runtime-created credentials, and re-applies the merged
// set to the Store.
func (m *Manager) Reload(base Config) error {
	if err := base.Validate(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.base = append([]Key(nil), base.Keys...)
	m.publicRead = append([]string(nil), base.PublicReadBuckets...)

	return m.store.Set(m.config())
}

// NewAccessKey returns an AWS-style 20-character access key ID.
func NewAccessKey() string {
	var b [10]byte

	_, _ = rand.Read(b[:])

	return "AKIA" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}

// NewSecretKey returns a 40-character secret access key.
func NewSecretKey() string {
	var b [30]byte

	_, _ = rand.Read(b[:])

	return base64.RawStdEncoding.EncodeToString(b[:])
}
