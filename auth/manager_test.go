package auth

import (
	"context"
	"encoding/json"
	"maps"
	"sync"
	"testing"

	"github.com/go-faster/errors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseConfig() Config {
	return Config{
		Keys: []Key{{
			AccessKey: "AKIACONFIG",
			SecretKey: "config-secret",
			Grants:    []Grant{{Pattern: "*", Permission: Admin}},
		}},
		PublicReadBuckets: []string{"public"},
	}
}

// memBackend is a Backend shared by the managers of one test, the way the
// engine's replicated table is shared by the nodes of a cluster.
type memBackend struct {
	mu   sync.Mutex
	keys map[string]json.RawMessage
	err  error
}

func newMemBackend() *memBackend { return &memBackend{keys: map[string]json.RawMessage{}} }

func (b *memBackend) AccessKeys(context.Context) (map[string]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.err != nil {
		return nil, b.err
	}

	return maps.Clone(b.keys), nil
}

func (b *memBackend) PutAccessKey(_ context.Context, id string, record json.RawMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.err != nil {
		return b.err
	}

	b.keys[id] = record

	return nil
}

func (b *memBackend) DeleteAccessKey(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.err != nil {
		return b.err
	}

	delete(b.keys, id)

	return nil
}

func TestManagerCreateListDelete(t *testing.T) {
	m, err := NewManager(baseConfig(), newMemBackend())
	require.NoError(t, err)

	// Base config key is listed and immutable.
	list := m.List()
	require.Len(t, list, 1)
	assert.Equal(t, "AKIACONFIG", list[0].AccessKey)
	assert.Equal(t, SourceConfig, list[0].Source)

	// Create with generated credentials.
	created, err := m.Create(t.Context(), CreateInput{Grants: []Grant{{Pattern: "uploads-*", Permission: Write}}})
	require.NoError(t, err)
	assert.True(t, len(created.AccessKey) == 20, "access key length")
	assert.NotEmpty(t, created.SecretKey)
	assert.False(t, created.CreatedAt.IsZero())

	// The new key authenticates against the live store.
	secret, ok := m.Store().Secret(created.AccessKey)
	require.True(t, ok)
	assert.Equal(t, created.SecretKey, secret)
	assert.True(t, m.Store().Allow(created.AccessKey, "uploads-1", ActionWrite))
	assert.False(t, m.Store().Allow(created.AccessKey, "other", ActionWrite))

	// Listed without the secret, sorted, marked managed.
	list = m.List()
	require.Len(t, list, 2)

	var managed *KeyInfo

	for i := range list {
		if list[i].Source == SourceManaged {
			managed = &list[i]
		}
	}

	require.NotNil(t, managed)
	assert.Equal(t, created.AccessKey, managed.AccessKey)

	// Delete removes it from the live store.
	require.NoError(t, m.Delete(t.Context(), created.AccessKey))
	_, ok = m.Store().Secret(created.AccessKey)
	assert.False(t, ok)
	assert.Len(t, m.List(), 1)
}

func TestManagerCreateExplicit(t *testing.T) {
	m, err := NewManager(baseConfig(), nil)
	require.NoError(t, err)

	created, err := m.Create(t.Context(), CreateInput{
		AccessKey: "AKIAEXPLICIT",
		SecretKey: "explicit-secret",
		Grants:    []Grant{{Pattern: "*", Permission: Read}},
	})
	require.NoError(t, err)
	assert.Equal(t, "AKIAEXPLICIT", created.AccessKey)
	assert.Equal(t, "explicit-secret", created.SecretKey)

	// Duplicate access key is rejected.
	_, err = m.Create(t.Context(), CreateInput{AccessKey: "AKIAEXPLICIT", SecretKey: "x"})
	assert.ErrorIs(t, err, ErrKeyExists)

	// Colliding with a config key is rejected.
	_, err = m.Create(t.Context(), CreateInput{AccessKey: "AKIACONFIG", SecretKey: "x"})
	assert.ErrorIs(t, err, ErrKeyExists)
}

func TestManagerDeleteErrors(t *testing.T) {
	m, err := NewManager(baseConfig(), nil)
	require.NoError(t, err)

	// Config keys cannot be deleted.
	err = m.Delete(t.Context(), "AKIACONFIG")
	assert.ErrorIs(t, err, ErrKeyImmutable)

	// Unknown keys report not found.
	err = m.Delete(t.Context(), "AKIAUNKNOWN")
	assert.ErrorIs(t, err, ErrKeyNotFound)
}

// TestManagerSharesKeysThroughTheBackend is the cluster case: a key created
// through one node is accepted by another once it refreshes, and a delete
// through the other is undone on the first.
func TestManagerSharesKeysThroughTheBackend(t *testing.T) {
	backend := newMemBackend()

	a, err := NewManager(baseConfig(), backend)
	require.NoError(t, err)

	b, err := NewManager(baseConfig(), backend)
	require.NoError(t, err)

	created, err := a.Create(t.Context(), CreateInput{
		AccessKey: "AKIASHARED",
		SecretKey: "shared-secret",
		Grants:    []Grant{{Pattern: "data", Permission: Write}},
	})
	require.NoError(t, err)

	_, ok := b.Store().Secret(created.AccessKey)
	assert.False(t, ok, "accepted before a refresh")

	require.NoError(t, b.Refresh(t.Context()))

	secret, ok := b.Store().Secret(created.AccessKey)
	require.True(t, ok, "not accepted after a refresh")
	assert.Equal(t, "shared-secret", secret)
	assert.True(t, b.Store().Allow("AKIASHARED", "data", ActionWrite))

	require.NoError(t, b.Delete(t.Context(), created.AccessKey))
	require.NoError(t, a.Refresh(t.Context()))

	_, ok = a.Store().Secret(created.AccessKey)
	assert.False(t, ok, "still accepted after a delete through another node")
}

// TestManagerKeepsKeysWhenTheBackendFails: an unreadable backend — a cluster
// without a layout yet, or out of quorum — must not revoke what this node
// already accepts, and a write that did not land must not apply here either.
func TestManagerKeepsKeysWhenTheBackendFails(t *testing.T) {
	backend := newMemBackend()

	m, err := NewManager(baseConfig(), backend)
	require.NoError(t, err)

	_, err = m.Create(t.Context(), CreateInput{AccessKey: "AKIAKEPT", SecretKey: "kept-secret"})
	require.NoError(t, err)

	backend.err = errors.New("no quorum")

	require.Error(t, m.Refresh(t.Context()))

	_, ok := m.Store().Secret("AKIAKEPT")
	assert.True(t, ok, "a failed refresh revoked a key")

	_, err = m.Create(t.Context(), CreateInput{AccessKey: "AKIALOST", SecretKey: "lost-secret"})
	require.Error(t, err)

	_, ok = m.Store().Secret("AKIALOST")
	assert.False(t, ok, "a key that was never stored is accepted")
}

// TestManagerConfigKeyWins: a runtime key of the same ID as a config key —
// created through another node whose config lacks it — does not replace what
// this node's config says.
func TestManagerConfigKeyWins(t *testing.T) {
	backend := newMemBackend()

	other, err := NewManager(Config{}, backend)
	require.NoError(t, err)

	_, err = other.Create(t.Context(), CreateInput{AccessKey: "AKIACONFIG", SecretKey: "runtime-secret"})
	require.NoError(t, err)

	m, err := NewManager(baseConfig(), backend)
	require.NoError(t, err)
	require.NoError(t, m.Refresh(t.Context()))

	secret, ok := m.Store().Secret("AKIACONFIG")
	require.True(t, ok)
	assert.Equal(t, "config-secret", secret)
	assert.Len(t, m.List(), 1)
}
