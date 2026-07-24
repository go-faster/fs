package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"

	"github.com/go-faster/errors"
)

// sealInfo is the HKDF "info" label binding the derived key to this purpose, so
// the same cluster secret used elsewhere (peer transport HMAC) never yields the
// same key. Changing it would make previously sealed secrets unreadable.
const sealInfo = "go-faster/fs cluster credential seal v1"

// Sealer seals and opens credential secrets with an AES-256-GCM key derived
// from the cluster secret via HKDF-SHA256. It exists so credentials stored in
// the cluster control plane (etcd) are encrypted at rest: a leaked etcd backup
// or a compromised control plane yields only ciphertext, not usable secret
// keys. The cluster secret is the sole key material; it never touches etcd.
//
// A Sealer is safe for concurrent use.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer derives the sealing key from clusterSecret and returns a Sealer.
// clusterSecret must be non-empty (the cluster secret is validated to be at
// least 16 bytes upstream).
func NewSealer(clusterSecret []byte) (*Sealer, error) {
	if len(clusterSecret) == 0 {
		return nil, errors.New("cluster secret is empty")
	}

	// HKDF over a fixed info label: the secret is high-entropy key material
	// already, so no salt is needed to stretch it — the label is what separates
	// this key from any other derived from the same secret.
	key, err := hkdf.Key(sha256.New, clusterSecret, nil, sealInfo, 32)
	if err != nil {
		return nil, errors.Wrap(err, "derive seal key")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.Wrap(err, "seal cipher")
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.Wrap(err, "seal aead")
	}

	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext and returns a base64 (raw std) blob of nonce ‖
// ciphertext. A fresh random nonce is used per call, so sealing the same secret
// twice yields different blobs.
func (s *Sealer) Seal(plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.Wrap(err, "read nonce")
	}

	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)

	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal. It fails if the blob is malformed, was sealed with a
// different cluster secret, or was tampered with (GCM authentication).
func (s *Sealer) Open(blob string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(blob)
	if err != nil {
		return "", errors.Wrap(err, "decode sealed secret")
	}

	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("sealed secret too short")
	}

	nonce, ciphertext := raw[:ns], raw[ns:]

	plaintext, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.Wrap(err, "open sealed secret (wrong cluster secret or corrupt)")
	}

	return string(plaintext), nil
}
