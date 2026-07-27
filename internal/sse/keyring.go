package sse

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"

	"github.com/go-faster/errors"
)

// Envelope encryption: each object gets its own random data key, and only that
// key is encrypted by the master key. Two properties follow, and both are the
// point of the arrangement.
//
// Rotating the master key rewrites wrapped data keys — a few dozen bytes per
// object in the sidecar — and never touches object bodies. A rotation is a
// metadata walk, not a re-upload of the store.
//
// And the master key is used on kilobytes rather than terabytes, which keeps it
// far from any usage limit and means a single compromised object body does not
// weaken any other object.

// WrappedKey is a data key sealed by a master key, as stored in a sidecar.
//
// KeyID names which master key sealed it, so a store part-way through a
// rotation stays readable: objects wrapped by the old key and the new one sit
// side by side and each says which it needs.
type WrappedKey struct {
	KeyID string `json:"key_id"`
	// Nonce and Ciphertext are the AES-GCM wrap of the data key.
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

// MasterKey is a key-encryption key and the identity it is known by.
type MasterKey struct {
	ID  string
	Key []byte
}

// MasterKeyID derives a master key's identity from the key itself, so an
// operator never has to name one and two deployments given the same key agree
// on what it is called.
//
// It is a truncated SHA-256 of a domain-separated hash of the key. Publishing
// it alongside every object is safe: it is a one-way function of a 256-bit
// secret, and it is truncated so it cannot serve as a verifier for a guess.
func MasterKeyID(key []byte) string {
	sum := sha256.Sum256(append([]byte("go-faster/fs sse master key id\x00"), key...))
	return hex.EncodeToString(sum[:8])
}

// NewMasterKey pairs a master key with its derived id.
func NewMasterKey(key []byte) (MasterKey, error) {
	if len(key) != KeySize {
		return MasterKey{}, errors.Errorf("master key must be %d bytes, got %d", KeySize, len(key))
	}

	return MasterKey{ID: MasterKeyID(key), Key: key}, nil
}

// Keyring holds the master key new objects are wrapped with, plus any older
// keys still needed to unwrap objects written before a rotation.
type Keyring struct {
	current MasterKey
	byID    map[string]MasterKey
}

// NewKeyring builds a keyring whose current key is current and which can also
// unwrap with each of previous.
func NewKeyring(current MasterKey, previous ...MasterKey) (*Keyring, error) {
	if len(current.Key) != KeySize {
		return nil, errors.Errorf("master key must be %d bytes, got %d", KeySize, len(current.Key))
	}

	kr := &Keyring{current: current, byID: map[string]MasterKey{current.ID: current}}

	for _, k := range previous {
		if len(k.Key) != KeySize {
			return nil, errors.Errorf("master key %q must be %d bytes, got %d", k.ID, KeySize, len(k.Key))
		}

		// A retired key that happens to equal the current one is not an error,
		// it is a no-op rotation; keep the current entry.
		if _, ok := kr.byID[k.ID]; !ok {
			kr.byID[k.ID] = k
		}
	}

	return kr, nil
}

// CurrentID is the id of the key new objects are wrapped with.
func (kr *Keyring) CurrentID() string { return kr.current.ID }

// Wrap seals a data key under the current master key.
func (kr *Keyring) Wrap(dek []byte) (WrappedKey, error) {
	return wrap(kr.current, dek)
}

// Unwrap recovers a data key, choosing the master key the wrap names.
func (kr *Keyring) Unwrap(w WrappedKey) ([]byte, error) {
	mk, ok := kr.byID[w.KeyID]
	if !ok {
		// Naming the key that is missing is what makes this recoverable: the
		// operator can put it back. Without the id the object is simply
		// unreadable with no way to tell which secret would fix it.
		return nil, errors.Errorf("no master key %q configured; the object needs it to be read", w.KeyID)
	}

	return unwrap(mk, w)
}

// Rewrap moves a wrapped key onto the current master key. It returns ok=false
// when the key is already current, so a rotation can skip writing a sidecar it
// would not have changed.
func (kr *Keyring) Rewrap(w WrappedKey) (WrappedKey, bool, error) {
	if w.KeyID == kr.current.ID {
		return w, false, nil
	}

	dek, err := kr.Unwrap(w)
	if err != nil {
		return WrappedKey{}, false, err
	}

	out, err := kr.Wrap(dek)
	if err != nil {
		return WrappedKey{}, false, err
	}

	return out, true, nil
}

func wrap(mk MasterKey, dek []byte) (WrappedKey, error) {
	if len(dek) != KeySize {
		return WrappedKey{}, errors.Errorf("data key must be %d bytes, got %d", KeySize, len(dek))
	}

	aead, err := newAEAD(mk.Key)
	if err != nil {
		return WrappedKey{}, err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return WrappedKey{}, errors.Wrap(err, "generate wrap nonce")
	}

	// The key id is authenticated, so a wrap cannot be relabelled to point at a
	// different master key without the tag failing.
	return WrappedKey{
		KeyID:      mk.ID,
		Nonce:      nonce,
		Ciphertext: aead.Seal(nil, nonce, dek, []byte(mk.ID)),
	}, nil
}

func unwrap(mk MasterKey, w WrappedKey) ([]byte, error) {
	aead, err := newAEAD(mk.Key)
	if err != nil {
		return nil, err
	}

	if len(w.Nonce) != aead.NonceSize() {
		return nil, errors.Errorf("wrapped key nonce must be %d bytes, got %d", aead.NonceSize(), len(w.Nonce))
	}

	dek, err := aead.Open(nil, w.Nonce, w.Ciphertext, []byte(w.KeyID))
	if err != nil {
		return nil, errors.Wrap(err, "unwrap data key: wrong master key or damaged sidecar")
	}

	if len(dek) != KeySize {
		return nil, errors.Errorf("unwrapped data key must be %d bytes, got %d", KeySize, len(dek))
	}

	return dek, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.Wrap(err, "new cipher")
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.Wrap(err, "new gcm")
	}

	return aead, nil
}
