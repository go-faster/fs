package sse_test

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/sse"
)

func newMaster(t *testing.T) sse.MasterKey {
	t.Helper()

	key, err := sse.NewKey()
	require.NoError(t, err)

	mk, err := sse.NewMasterKey(key)
	require.NoError(t, err)

	return mk
}

func TestWrapRoundTrip(t *testing.T) {
	mk := newMaster(t)

	kr, err := sse.NewKeyring(mk)
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	w, err := kr.Wrap(dek)
	require.NoError(t, err)
	require.Equal(t, mk.ID, w.KeyID)
	require.NotEmpty(t, w.Nonce)

	require.False(t, bytes.Contains(w.Ciphertext, dek),
		"the data key must not appear in its own wrapping")

	got, err := kr.Unwrap(w)
	require.NoError(t, err)
	require.Equal(t, dek, got)
}

// TestWrapIsFresh: wrapping the same key twice must not produce the same
// bytes, or the nonce is not being drawn per wrap.
func TestWrapIsFresh(t *testing.T) {
	kr, err := sse.NewKeyring(newMaster(t))
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	a, err := kr.Wrap(dek)
	require.NoError(t, err)

	b, err := kr.Wrap(dek)
	require.NoError(t, err)

	require.NotEqual(t, a.Nonce, b.Nonce)
	require.NotEqual(t, a.Ciphertext, b.Ciphertext)
}

func TestMasterKeyIDIsStableAndDistinct(t *testing.T) {
	a, err := sse.NewKey()
	require.NoError(t, err)

	b, err := sse.NewKey()
	require.NoError(t, err)

	require.Equal(t, sse.MasterKeyID(a), sse.MasterKeyID(a), "id must be a function of the key")
	require.NotEqual(t, sse.MasterKeyID(a), sse.MasterKeyID(b))

	require.NotContains(t, sse.MasterKeyID(a), string(a), "the id must not leak the key")
}

func TestUnwrapWithWrongMasterKey(t *testing.T) {
	one, err := sse.NewKeyring(newMaster(t))
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	w, err := one.Wrap(dek)
	require.NoError(t, err)

	// A keyring that does not hold the key the wrap names.
	two, err := sse.NewKeyring(newMaster(t))
	require.NoError(t, err)

	_, err = two.Unwrap(w)
	require.Error(t, err)
	require.Contains(t, err.Error(), w.KeyID,
		"the error must name the missing key, or an operator cannot tell which secret would fix it")
}

// TestUnwrapRelabelled: rewriting the key id in a sidecar must not let a wrap
// be opened by a different master key.
func TestUnwrapRelabelled(t *testing.T) {
	old := newMaster(t)
	current := newMaster(t)

	kr, err := sse.NewKeyring(current, old)
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	w, err := sse.NewKeyring(old)
	require.NoError(t, err)

	wrapped, err := w.Wrap(dek)
	require.NoError(t, err)

	relabelled := wrapped
	relabelled.KeyID = current.ID

	_, err = kr.Unwrap(relabelled)
	require.Error(t, err)
}

func TestUnwrapTampered(t *testing.T) {
	kr, err := sse.NewKeyring(newMaster(t))
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	w, err := kr.Wrap(dek)
	require.NoError(t, err)

	damaged := w
	damaged.Ciphertext = bytes.Clone(w.Ciphertext)
	damaged.Ciphertext[0] ^= 0x01

	_, err = kr.Unwrap(damaged)
	require.Error(t, err)
}

// TestRotation is the drill the design promises: a new master key, old objects
// still readable, and rewrapping touches only the sidecar.
func TestRotation(t *testing.T) {
	oldKey := newMaster(t)

	before, err := sse.NewKeyring(oldKey)
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	base, err := sse.NewNonceBase()
	require.NoError(t, err)

	// An object written before the rotation.
	c, err := sse.New(dek, base, 0)
	require.NoError(t, err)

	plain := bytes.Repeat([]byte("payload"), 3000)
	ct := encrypt(t, c, plain)

	wrapped, err := before.Wrap(dek)
	require.NoError(t, err)
	require.Equal(t, oldKey.ID, wrapped.KeyID)

	// Rotate: a new current key, the old one retained for reading.
	newKey := newMaster(t)

	after, err := sse.NewKeyring(newKey, oldKey)
	require.NoError(t, err)

	// Readable across the rotation, before any rewrap.
	recovered, err := after.Unwrap(wrapped)
	require.NoError(t, err)
	require.Equal(t, dek, recovered)

	rewrapped, changed, err := after.Rewrap(wrapped)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, newKey.ID, rewrapped.KeyID)

	// The body was never touched, and still decrypts under the rewrapped key.
	rekey, err := after.Unwrap(rewrapped)
	require.NoError(t, err)

	rc, err := sse.New(rekey, base, 0)
	require.NoError(t, err)

	got, err := io.ReadAll(sse.NewReader(bytes.NewReader(ct), rc, int64(len(plain))))
	require.NoError(t, err)
	require.Equal(t, plain, got)

	// Rewrapping something already current reports no change, so a rotation
	// pass can skip rewriting sidecars it would not have altered.
	_, changed, err = after.Rewrap(rewrapped)
	require.NoError(t, err)
	require.False(t, changed)
}

// TestRotationDropsOldKey: once the old key is out of the keyring, anything
// still wrapped by it is unreadable — the reason a rotation must finish before
// the retired key is discarded.
func TestRotationDropsOldKey(t *testing.T) {
	oldKey := newMaster(t)
	newKey := newMaster(t)

	before, err := sse.NewKeyring(oldKey)
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	wrapped, err := before.Wrap(dek)
	require.NoError(t, err)

	onlyNew, err := sse.NewKeyring(newKey)
	require.NoError(t, err)

	_, err = onlyNew.Unwrap(wrapped)
	require.Error(t, err)
}

func TestKeyringRejectsBadKeySize(t *testing.T) {
	_, err := sse.NewKeyring(sse.MasterKey{ID: "short", Key: []byte("too short")})
	require.Error(t, err)

	good := newMaster(t)

	_, err = sse.NewKeyring(good, sse.MasterKey{ID: "short", Key: []byte("too short")})
	require.Error(t, err)
}

// TestWrappedKeySurvivesJSON: the wrap travels in the sidecar, so it has to
// round-trip through the encoding the sidecar uses.
func TestWrappedKeySurvivesJSON(t *testing.T) {
	kr, err := sse.NewKeyring(newMaster(t))
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	w, err := kr.Wrap(dek)
	require.NoError(t, err)

	data, err := json.Marshal(w)
	require.NoError(t, err)

	var back sse.WrappedKey
	require.NoError(t, json.Unmarshal(data, &back))

	got, err := kr.Unwrap(back)
	require.NoError(t, err)
	require.Equal(t, dek, got)
}
