package main

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/sse"
)

func writeKeyFile(t *testing.T, name string, content []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, content, 0o600))

	return path
}

// TestMasterKeyEncodings: an operator hands over a key in whatever shape their
// secret store produces, and should not have to discover which one was wanted.
func TestMasterKeyEncodings(t *testing.T) {
	raw, err := sse.NewKey()
	require.NoError(t, err)

	for name, content := range map[string][]byte{
		"raw":              raw,
		"hex":              []byte(hex.EncodeToString(raw)),
		"hex with newline": []byte(hex.EncodeToString(raw) + "\n"),
		"base64":           []byte(base64.StdEncoding.EncodeToString(raw)),
		"base64 newline":   []byte(base64.StdEncoding.EncodeToString(raw) + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := EncryptionConfig{MasterKeyFile: writeKeyFile(t, "key", content)}

			kr, err := cfg.Keyring()
			require.NoError(t, err)
			require.NotNil(t, kr)
			require.Equal(t, sse.MasterKeyID(raw), kr.CurrentID(),
				"every encoding must yield the same key, and so the same id")
		})
	}
}

func TestMasterKeyRejectsWrongLength(t *testing.T) {
	for name, content := range map[string][]byte{
		"too short hex": []byte("abcd"),
		"empty":         {},
		"not a key":     []byte("hello there, this is not a key at all!!"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := EncryptionConfig{MasterKeyFile: writeKeyFile(t, "key", content)}

			_, err := cfg.Keyring()
			require.Error(t, err)
		})
	}
}

func TestMasterKeyMissingFile(t *testing.T) {
	cfg := EncryptionConfig{MasterKeyFile: filepath.Join(t.TempDir(), "absent")}

	_, err := cfg.Keyring()
	require.Error(t, err)
	require.Contains(t, err.Error(), "absent", "the error must name the file that could not be read")
}

// TestMasterKeyFromEnv covers the path most orchestrators can actually use.
func TestMasterKeyFromEnv(t *testing.T) {
	raw, err := sse.NewKey()
	require.NoError(t, err)

	t.Setenv(masterKeyEnv, hex.EncodeToString(raw))

	cfg := EncryptionConfig{}
	require.True(t, cfg.Enabled(), "the environment alone must be enough to turn encryption on")

	kr, err := cfg.Keyring()
	require.NoError(t, err)
	require.Equal(t, sse.MasterKeyID(raw), kr.CurrentID())
}

// TestEnvOverridesFile: when both are given the environment wins, so a
// deployment can override a baked-in config without editing it.
func TestEnvOverridesFile(t *testing.T) {
	fileKey, err := sse.NewKey()
	require.NoError(t, err)

	envKey, err := sse.NewKey()
	require.NoError(t, err)

	t.Setenv(masterKeyEnv, hex.EncodeToString(envKey))

	cfg := EncryptionConfig{MasterKeyFile: writeKeyFile(t, "key", fileKey)}

	kr, err := cfg.Keyring()
	require.NoError(t, err)
	require.Equal(t, sse.MasterKeyID(envKey), kr.CurrentID())
}

// TestPreviousKeysLoad is the rotation window: retired keys decrypt old
// objects while new ones are wrapped with the current key.
func TestPreviousKeysLoad(t *testing.T) {
	current, err := sse.NewKey()
	require.NoError(t, err)

	old, err := sse.NewKey()
	require.NoError(t, err)

	cfg := EncryptionConfig{
		MasterKeyFile:    writeKeyFile(t, "current", current),
		PreviousKeyFiles: []string{writeKeyFile(t, "old", old)},
	}

	kr, err := cfg.Keyring()
	require.NoError(t, err)
	require.Equal(t, sse.MasterKeyID(current), kr.CurrentID())

	// Something wrapped by the retired key still opens.
	retired, err := sse.NewMasterKey(old)
	require.NoError(t, err)

	oldRing, err := sse.NewKeyring(retired)
	require.NoError(t, err)

	dek, err := sse.NewKey()
	require.NoError(t, err)

	wrapped, err := oldRing.Wrap(dek)
	require.NoError(t, err)

	got, err := kr.Unwrap(wrapped)
	require.NoError(t, err)
	require.Equal(t, dek, got)
}

// TestEncryptionOffByDefault: no key configured means no keyring, and a store
// built from it stores plaintext. Encryption is never switched on implicitly.
func TestEncryptionOffByDefault(t *testing.T) {
	cfg := EncryptionConfig{}
	require.False(t, cfg.Enabled())

	kr, err := cfg.Keyring()
	require.NoError(t, err)
	require.Nil(t, kr)
}

func TestEncryptionValidate(t *testing.T) {
	key, err := sse.NewKey()
	require.NoError(t, err)

	path := writeKeyFile(t, "key", key)

	require.NoError(t, EncryptionConfig{}.Validate())
	require.NoError(t, EncryptionConfig{MasterKeyFile: path}.Validate())
	require.NoError(t, EncryptionConfig{MasterKeyFile: path, DefaultAlgorithm: sse.Algorithm}.Validate())

	// A default with no key would fail on the first write instead of at load,
	// which is the wrong time to find out.
	require.Error(t, EncryptionConfig{DefaultAlgorithm: sse.Algorithm}.Validate())

	// Naming an algorithm this server does not implement must fail loudly.
	require.Error(t, EncryptionConfig{MasterKeyFile: path, DefaultAlgorithm: "aws:kms"}.Validate())

	// Retired keys without a current one is a misconfiguration, not a rotation.
	require.Error(t, EncryptionConfig{PreviousKeyFiles: []string{path}}.Validate())
}

// TestMasterKeyPathIsRelativeToConfig: a config and the key it names travel
// together, so the pair must work from any working directory. This broke a
// consumer that checked the repository out as a subdirectory and ran from its
// own root — the server failed to start, and failed late.
func TestMasterKeyPathIsRelativeToConfig(t *testing.T) {
	dir := t.TempDir()

	raw, err := sse.NewKey()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "master.key"), []byte(hex.EncodeToString(raw)), 0o600))

	cfgPath := filepath.Join(dir, "server.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(
		"server:\n  addr: \":8080\"\nstorage:\n  root: \"/tmp/x\"\n  type: \"filesystem\"\n"+
			"observability:\n  service_name: \"go-faster/fs\"\n"+
			"encryption:\n  master_key_file: \"master.key\"\n"), 0o600))

	// Load from a working directory that is not the config's.
	cfg, err := LoadConfig(cfgPath)
	require.NoError(t, err)

	kr, err := cfg.Encryption.Keyring()
	require.NoError(t, err, "the key beside the config was not found")
	require.Equal(t, sse.MasterKeyID(raw), kr.CurrentID())
}

// TestMasterKeyAbsolutePathUnchanged: an absolute path means what it says.
func TestMasterKeyAbsolutePathUnchanged(t *testing.T) {
	dir := t.TempDir()

	raw, err := sse.NewKey()
	require.NoError(t, err)

	keyPath := filepath.Join(dir, "abs.key")
	require.NoError(t, os.WriteFile(keyPath, []byte(hex.EncodeToString(raw)), 0o600))

	cfgPath := filepath.Join(t.TempDir(), "server.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(
		"server:\n  addr: \":8080\"\nstorage:\n  root: \"/tmp/x\"\n  type: \"filesystem\"\n"+
			"observability:\n  service_name: \"go-faster/fs\"\n"+
			// Single-quoted: YAML does not read backslash escapes there, and on
			// Windows an absolute path is full of them.
			"encryption:\n  master_key_file: '"+keyPath+"'\n"), 0o600))

	cfg, err := LoadConfig(cfgPath)
	require.NoError(t, err)
	require.Equal(t, keyPath, cfg.Encryption.MasterKeyFile)

	kr, err := cfg.Encryption.Keyring()
	require.NoError(t, err)
	require.Equal(t, sse.MasterKeyID(raw), kr.CurrentID())
}
