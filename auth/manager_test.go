package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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

func TestManagerCreateListDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")

	m, err := NewManager(baseConfig(), path)
	require.NoError(t, err)

	// Base config key is listed and immutable.
	list := m.List()
	require.Len(t, list, 1)
	assert.Equal(t, "AKIACONFIG", list[0].AccessKey)
	assert.Equal(t, SourceConfig, list[0].Source)

	// Create with generated credentials.
	created, err := m.Create(CreateInput{Grants: []Grant{{Pattern: "uploads-*", Permission: Write}}})
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
	require.NoError(t, m.Delete(created.AccessKey))
	_, ok = m.Store().Secret(created.AccessKey)
	assert.False(t, ok)
	assert.Len(t, m.List(), 1)
}

func TestManagerCreateExplicit(t *testing.T) {
	m, err := NewManager(baseConfig(), "")
	require.NoError(t, err)

	created, err := m.Create(CreateInput{
		AccessKey: "AKIAEXPLICIT",
		SecretKey: "explicit-secret",
		Grants:    []Grant{{Pattern: "*", Permission: Read}},
	})
	require.NoError(t, err)
	assert.Equal(t, "AKIAEXPLICIT", created.AccessKey)
	assert.Equal(t, "explicit-secret", created.SecretKey)

	// Duplicate access key is rejected.
	_, err = m.Create(CreateInput{AccessKey: "AKIAEXPLICIT", SecretKey: "x"})
	assert.ErrorIs(t, err, ErrKeyExists)

	// Colliding with a config key is rejected.
	_, err = m.Create(CreateInput{AccessKey: "AKIACONFIG", SecretKey: "x"})
	assert.ErrorIs(t, err, ErrKeyExists)
}

func TestManagerDeleteErrors(t *testing.T) {
	m, err := NewManager(baseConfig(), "")
	require.NoError(t, err)

	// Config keys cannot be deleted.
	err = m.Delete("AKIACONFIG")
	assert.ErrorIs(t, err, ErrKeyImmutable)

	// Unknown keys report not found.
	err = m.Delete("AKIAUNKNOWN")
	assert.ErrorIs(t, err, ErrKeyNotFound)
}

func TestManagerPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")

	m, err := NewManager(baseConfig(), path)
	require.NoError(t, err)

	created, err := m.Create(CreateInput{
		AccessKey: "AKIAPERSIST",
		SecretKey: "persist-secret",
		Grants:    []Grant{{Pattern: "data", Permission: Admin}},
	})
	require.NoError(t, err)

	// A fresh manager over the same file recovers the managed key.
	m2, err := NewManager(baseConfig(), path)
	require.NoError(t, err)

	secret, ok := m2.Store().Secret(created.AccessKey)
	require.True(t, ok)
	assert.Equal(t, "persist-secret", secret)
	assert.True(t, m2.Store().Allow("AKIAPERSIST", "data", ActionWrite))

	list := m2.List()
	assert.Len(t, list, 2)
}

func TestManagerPersistenceFilePerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions are not enforced on Windows")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")

	m, err := NewManager(baseConfig(), path)
	require.NoError(t, err)

	_, err = m.Create(CreateInput{AccessKey: "AKIAPERM", SecretKey: "s", Grants: []Grant{{Pattern: "*", Permission: Read}}})
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, "-rw-------", info.Mode().String())
}
