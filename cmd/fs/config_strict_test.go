package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestShippedConfigsLoad: every config in the repository loads, now that an
// unknown key refuses to start the server.
func TestShippedConfigsLoad(t *testing.T) {
	for _, f := range []string{
		"../../config.yaml", "../../config.dev.yaml", "../../config.production.yaml", "../../.github/s3tests/server.yaml",
	} {
		_, err := LoadConfig(f)
		require.NoError(t, err, f)
	}

	// What fs s3 --generate-config prints.
	generated, err := yaml.Marshal(DefaultConfig())
	require.NoError(t, err)

	p := filepath.Join(t.TempDir(), "generated.yaml")
	require.NoError(t, os.WriteFile(p, generated, 0o600))

	_, err = LoadConfig(p)
	require.NoError(t, err, "the generated config")

	// The Helm chart renders its config value as the server's config file.
	for _, f := range []string{"../../helm/go-faster-fs/values.yaml", "../../helm/go-faster-fs/values-production.yaml"} {
		raw, err := os.ReadFile(f) //nolint:gosec // Test fixture.
		require.NoError(t, err)

		var values struct {
			Config map[string]any `yaml:"config"`
		}
		require.NoError(t, yaml.Unmarshal(raw, &values), f)

		if values.Config == nil {
			continue
		}

		rendered, err := yaml.Marshal(values.Config)
		require.NoError(t, err)

		p := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(p, rendered, 0o600))

		_, err = LoadConfig(p)
		require.NoError(t, err, "%s config", f)
	}
}

// TestConfigRefusesUnknownKeys: a setting this binary does not know —
// misspelled, or one a release removed — is an error naming it, never
// silently ignored.
func TestConfigRefusesUnknownKeys(t *testing.T) {
	dir := t.TempDir()

	for name, body := range map[string]string{
		"removed section": "etcd:\n  endpoints: [\"x:2379\"]\n",
		"misspelled key":  "storage:\n  rooot: /data\n",
	} {
		p := filepath.Join(dir, "c.yaml")
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))

		_, err := LoadConfig(p)
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "not found", name)
	}

	empty := filepath.Join(dir, "empty.yaml")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))

	_, err := LoadConfig(empty)
	require.NoError(t, err, "an empty file is the defaults")
}
