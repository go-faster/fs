package auth

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
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
	// (editable and deletable, persisted to disk).
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

// managedKey is the persisted form of a runtime-created credential (secret
// included).
type managedKey struct {
	AccessKey string    `json:"access_key"`
	SecretKey string    `json:"secret_key"`
	Grants    []Grant   `json:"grants"`
	CreatedAt time.Time `json:"created_at"`
}

// Manager owns the live auth Store and adds runtime CRUD over credentials.
// Config/env credentials form a read-only base; runtime-created credentials are
// persisted to a JSON file and merged over the base. Every mutation rebuilds the
// Store snapshot atomically, so live requests immediately see the change.
type Manager struct {
	store *Store
	path  string

	mu         sync.Mutex
	base       []Key // config/env credentials (read-only)
	publicRead []string
	managed    map[string]managedKey
	now        func() time.Time
}

// NewManager builds a Manager from a base Config (config/env credentials) and a
// persistence path for runtime-created credentials. It loads any previously
// persisted credentials and applies the merged set to a new Store. path may be
// empty to keep runtime credentials in memory only.
func NewManager(base Config, path string) (*Manager, error) {
	if err := base.Validate(); err != nil {
		return nil, err
	}

	m := &Manager{
		path:       path,
		base:       append([]Key(nil), base.Keys...),
		publicRead: append([]string(nil), base.PublicReadBuckets...),
		managed:    make(map[string]managedKey),
		now:        time.Now,
	}

	if err := m.load(); err != nil {
		return nil, err
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
		keys = append(keys, Key{AccessKey: mk.AccessKey, SecretKey: mk.SecretKey, Grants: mk.Grants})
	}

	return Config{Keys: keys, PublicReadBuckets: m.publicRead}
}

// baseAccessKeys reports which access keys come from the static config.
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

// Create adds a runtime credential, generating the access key and/or secret when
// not supplied, persists it, and applies it to the live Store. The returned
// Created carries the secret (the only time it is exposed).
func (m *Manager) Create(in CreateInput) (*Created, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	access := in.AccessKey
	if access == "" {
		access = generateAccessKey()
	}

	secret := in.SecretKey
	if secret == "" {
		secret = generateSecretKey()
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

	m.managed[access] = mk

	if err := m.applyLocked(); err != nil {
		delete(m.managed, access)
		return nil, err
	}

	return &Created{AccessKey: access, SecretKey: secret, Grants: mk.Grants, CreatedAt: mk.CreatedAt}, nil
}

// Delete removes a runtime credential. Config credentials cannot be deleted.
func (m *Manager) Delete(accessKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.isBase(accessKey) {
		return errors.Wrapf(ErrKeyImmutable, "access key %q", accessKey)
	}

	if _, ok := m.managed[accessKey]; !ok {
		return errors.Wrapf(ErrKeyNotFound, "access key %q", accessKey)
	}

	prev := m.managed[accessKey]
	delete(m.managed, accessKey)

	if err := m.applyLocked(); err != nil {
		m.managed[accessKey] = prev
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

	return m.applyLocked()
}

// applyLocked persists the managed set and re-applies the merged config to the
// Store. The caller must hold m.mu.
func (m *Manager) applyLocked() error {
	if err := m.store.Set(m.config()); err != nil {
		return err
	}

	return m.persist()
}

// persist writes the managed credentials to disk atomically (0600). A no-op when
// no path is configured. The caller must hold m.mu.
func (m *Manager) persist() error {
	if m.path == "" {
		return nil
	}

	keys := make([]managedKey, 0, len(m.managed))
	for _, mk := range m.managed {
		keys = append(keys, mk)
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i].AccessKey < keys[j].AccessKey })

	data, err := json.MarshalIndent(struct {
		Keys []managedKey `json:"keys"`
	}{keys}, "", "  ")
	if err != nil {
		return errors.Wrap(err, "marshal managed keys")
	}

	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return errors.Wrap(err, "create keys dir")
	}

	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return errors.Wrap(err, "write managed keys")
	}

	if err := os.Rename(tmp, m.path); err != nil {
		return errors.Wrap(err, "replace managed keys")
	}

	return nil
}

// load reads persisted managed credentials from disk. The caller must hold
// m.mu (or be in construction).
func (m *Manager) load() error {
	if m.path == "" {
		return nil
	}

	data, err := os.ReadFile(m.path) //nolint:gosec // Operator-configured path.
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return errors.Wrap(err, "read managed keys")
	}

	var doc struct {
		Keys []managedKey `json:"keys"`
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		return errors.Wrap(err, "parse managed keys")
	}

	for _, mk := range doc.Keys {
		if mk.AccessKey == "" || mk.SecretKey == "" {
			continue
		}

		m.managed[mk.AccessKey] = mk
	}

	return nil
}

// generateAccessKey returns an AWS-style 20-character access key ID.
func generateAccessKey() string {
	var b [10]byte

	_, _ = rand.Read(b[:])

	return "AKIA" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}

// generateSecretKey returns a 40-character secret access key.
func generateSecretKey() string {
	var b [30]byte

	_, _ = rand.Read(b[:])

	return base64.RawStdEncoding.EncodeToString(b[:])
}
