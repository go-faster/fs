package main

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/sse"
)

// EncryptionConfig configures server-side encryption of object bodies at rest
// (SSE-S3).
//
// Encryption is off unless a master key is given. It is not defaulted to on
// with a generated key: a key this server invented and stored next to the data
// protects against nothing an attacker holding the disk cannot undo, and would
// give operators a false assurance. The key has to come from somewhere the
// disk does not.
type EncryptionConfig struct {
	// MasterKeyFile is a file holding the 32-byte master key, as 64 hex
	// characters, standard base64, or 32 raw bytes. Surrounding whitespace is
	// ignored so a file written by `echo` works.
	MasterKeyFile string `yaml:"master_key_file,omitempty"`

	// PreviousKeyFiles are master keys retained only to decrypt objects
	// written before a rotation. New objects are always wrapped with
	// MasterKeyFile's key. Remove an entry once `fs encrypt rotate` reports no
	// objects left under it — before that, removing it makes those objects
	// unreadable.
	PreviousKeyFiles []string `yaml:"previous_key_files,omitempty"`

	// DefaultAlgorithm, when set to "AES256", encrypts every object whose
	// request does not say otherwise, as though every bucket carried a default
	// encryption configuration. Empty leaves encryption per request and per
	// bucket.
	DefaultAlgorithm string `yaml:"default_algorithm,omitempty"`
}

// masterKeyEnv overrides EncryptionConfig.MasterKeyFile. It exists because a
// key belongs in a secret store, and every orchestrator can inject one as an
// environment variable while few can place a file.
const masterKeyEnv = "FS_MASTER_KEY"

// Enabled reports whether any master key is configured.
func (c EncryptionConfig) Enabled() bool {
	return c.MasterKeyFile != "" || os.Getenv(masterKeyEnv) != ""
}

// Validate checks what can be checked without reading the keys.
func (c EncryptionConfig) Validate() error {
	if c.DefaultAlgorithm != "" && c.DefaultAlgorithm != sse.Algorithm {
		return errors.Errorf(
			"encryption.default_algorithm must be %q or empty, got %q", sse.Algorithm, c.DefaultAlgorithm)
	}

	if c.DefaultAlgorithm != "" && !c.Enabled() {
		return errors.New(
			"encryption.default_algorithm is set but no master key is configured; " +
				"set encryption.master_key_file or " + masterKeyEnv)
	}

	if len(c.PreviousKeyFiles) > 0 && !c.Enabled() {
		return errors.New("encryption.previous_key_files is set but no current master key is configured")
	}

	return nil
}

// Keyring loads the master keys, or returns nil when encryption is off.
func (c EncryptionConfig) Keyring() (*sse.Keyring, error) {
	if !c.Enabled() {
		return nil, nil //nolint:nilnil // No keyring configured is not an error.
	}

	current, err := c.currentKey()
	if err != nil {
		return nil, err
	}

	previous := make([]sse.MasterKey, 0, len(c.PreviousKeyFiles))

	for _, path := range c.PreviousKeyFiles {
		key, err := readMasterKeyFile(path)
		if err != nil {
			return nil, err
		}

		mk, err := sse.NewMasterKey(key)
		if err != nil {
			return nil, errors.Wrapf(err, "previous master key %q", path)
		}

		previous = append(previous, mk)
	}

	kr, err := sse.NewKeyring(current, previous...)
	if err != nil {
		return nil, errors.Wrap(err, "build keyring")
	}

	return kr, nil
}

// currentKey loads the key new objects are wrapped with, preferring the
// environment over the file.
func (c EncryptionConfig) currentKey() (sse.MasterKey, error) {
	if raw := os.Getenv(masterKeyEnv); raw != "" {
		key, err := decodeMasterKey([]byte(raw))
		if err != nil {
			return sse.MasterKey{}, errors.Wrapf(err, "$%s", masterKeyEnv)
		}

		mk, err := sse.NewMasterKey(key)
		if err != nil {
			return sse.MasterKey{}, errors.Wrapf(err, "$%s", masterKeyEnv)
		}

		return mk, nil
	}

	key, err := readMasterKeyFile(c.MasterKeyFile)
	if err != nil {
		return sse.MasterKey{}, err
	}

	mk, err := sse.NewMasterKey(key)
	if err != nil {
		return sse.MasterKey{}, errors.Wrapf(err, "master key %q", c.MasterKeyFile)
	}

	return mk, nil
}

func readMasterKeyFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The path is the operator's own configuration.
	if err != nil {
		return nil, errors.Wrapf(err, "read master key %q", path)
	}

	key, err := decodeMasterKey(data)
	if err != nil {
		return nil, errors.Wrapf(err, "master key %q", path)
	}

	return key, nil
}

// decodeMasterKey accepts the three shapes a 32-byte key is realistically
// handed over in, so an operator does not have to discover which one this
// server wanted.
func decodeMasterKey(data []byte) ([]byte, error) {
	// Raw bytes are checked before the text encodings, since a 32-byte random
	// file is a legitimate key and could coincidentally decode as something
	// else only if it were also valid hex or base64, which the length rules out
	// for hex and makes vanishingly unlikely for base64.
	if len(data) == sse.KeySize {
		return data, nil
	}

	text := strings.TrimSpace(string(data))

	if decoded, err := hex.DecodeString(text); err == nil && len(decoded) == sse.KeySize {
		return decoded, nil
	}

	if decoded, err := base64.StdEncoding.DecodeString(text); err == nil && len(decoded) == sse.KeySize {
		return decoded, nil
	}

	return nil, errors.Errorf(
		"must be a %d-byte key as %d hex characters, standard base64, or raw bytes",
		sse.KeySize, sse.KeySize*2)
}
