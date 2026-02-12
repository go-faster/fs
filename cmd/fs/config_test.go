package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.Equal(t, ":8080", cfg.Server.Addr)
	assert.Equal(t, 30*time.Second, cfg.Server.ReadTimeout)
	assert.Equal(t, 30*time.Second, cfg.Server.WriteTimeout)
	assert.Equal(t, 120*time.Second, cfg.Server.IdleTimeout)
	assert.Equal(t, "/health", cfg.Server.HealthPath)

	assert.Equal(t, ".s3data", cfg.Storage.Root)
	assert.Equal(t, "filesystem", cfg.Storage.Type)

	assert.Equal(t, "go-faster/fs", cfg.Observability.ServiceName)
	assert.True(t, cfg.Observability.EnableRequestLogging)
	assert.True(t, cfg.Observability.EnableMetrics)
	assert.True(t, cfg.Observability.EnableTracing)
}

func TestLoadConfig_NonExistent(t *testing.T) {
	// Loading non-existent file should return default config
	cfg, err := LoadConfig("non-existent-file.yaml")
	require.NoError(t, err)

	// Should have default values
	assert.Equal(t, ":8080", cfg.Server.Addr)
	assert.Equal(t, ".s3data", cfg.Storage.Root)
}

func TestLoadConfig_Empty(t *testing.T) {
	// Empty path should return default config
	cfg, err := LoadConfig("")
	require.NoError(t, err)

	// Should have default values
	assert.Equal(t, ":8080", cfg.Server.Addr)
	assert.Equal(t, ".s3data", cfg.Storage.Root)
}

func TestLoadConfig_Valid(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  addr: ":9000"
  read_timeout: 60s
  write_timeout: 90s
  idle_timeout: 180s
  health_path: "/healthz"

storage:
  root: "/tmp/test-s3"
  type: "filesystem"

observability:
  service_name: "test-service"
  enable_request_logging: false
  enable_metrics: false
  enable_tracing: false
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	// Verify values
	assert.Equal(t, ":9000", cfg.Server.Addr)
	assert.Equal(t, 60*time.Second, cfg.Server.ReadTimeout)
	assert.Equal(t, 90*time.Second, cfg.Server.WriteTimeout)
	assert.Equal(t, 180*time.Second, cfg.Server.IdleTimeout)
	assert.Equal(t, "/healthz", cfg.Server.HealthPath)

	assert.Equal(t, "/tmp/test-s3", cfg.Storage.Root)
	assert.Equal(t, "filesystem", cfg.Storage.Type)

	assert.Equal(t, "test-service", cfg.Observability.ServiceName)
	assert.False(t, cfg.Observability.EnableRequestLogging)
	assert.False(t, cfg.Observability.EnableMetrics)
	assert.False(t, cfg.Observability.EnableTracing)
}

func TestLoadConfig_PartialOverride(t *testing.T) {
	// Create a config with only some values set
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  addr: ":9000"

storage:
  root: "/tmp/test-s3"
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	// Load the config
	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	// Verify overridden values
	assert.Equal(t, ":9000", cfg.Server.Addr)
	assert.Equal(t, "/tmp/test-s3", cfg.Storage.Root)

	// Verify default values for non-overridden fields
	assert.Equal(t, 30*time.Second, cfg.Server.ReadTimeout)
	assert.Equal(t, "/health", cfg.Server.HealthPath)
	assert.Equal(t, "go-faster/fs", cfg.Observability.ServiceName)
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Write invalid YAML
	err := os.WriteFile(configPath, []byte("invalid: yaml: content:"), 0644)
	require.NoError(t, err)

	// Should return error
	_, err = LoadConfig(configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config")
}

func TestValidate_Success(t *testing.T) {
	cfg := DefaultConfig()
	err := cfg.Validate()
	require.NoError(t, err)
}

func TestValidate_EmptyAddr(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.Addr = ""

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.addr is required")
}

func TestValidate_EmptyRoot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Storage.Root = ""

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage.root is required")
}

func TestValidate_InvalidStorageType(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Storage.Type = "s3"

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported storage type")
}

func TestValidate_InvalidTimeouts(t *testing.T) {
	testCases := []struct {
		name     string
		modify   func(*Config)
		errorMsg string
	}{
		{
			name: "zero read timeout",
			modify: func(c *Config) {
				c.Server.ReadTimeout = 0
			},
			errorMsg: "server.read_timeout must be positive",
		},
		{
			name: "negative write timeout",
			modify: func(c *Config) {
				c.Server.WriteTimeout = -1 * time.Second
			},
			errorMsg: "server.write_timeout must be positive",
		},
		{
			name: "zero idle timeout",
			modify: func(c *Config) {
				c.Server.IdleTimeout = 0
			},
			errorMsg: "server.idle_timeout must be positive",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.modify(&cfg)

			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errorMsg)
		})
	}
}

func TestValidate_EmptyServiceName(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Observability.ServiceName = ""

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "observability.service_name is required")
}

func TestValidate_BucketNames(t *testing.T) {
	testCases := []struct {
		name     string
		buckets  []string
		wantErr  bool
		errorMsg string
	}{
		{
			name:    "valid bucket names",
			buckets: []string{"my-bucket", "test-bucket", "uploads"},
			wantErr: false,
		},
		{
			name:     "empty bucket name",
			buckets:  []string{"valid-bucket", "", "another-bucket"},
			wantErr:  true,
			errorMsg: "cannot contain empty bucket names",
		},
		{
			name:     "bucket name too short",
			buckets:  []string{"ab"},
			wantErr:  true,
			errorMsg: "must be between 3 and 63 characters",
		},
		{
			name:     "bucket name too long",
			buckets:  []string{"this-is-a-very-long-bucket-name-that-exceeds-the-maximum-allowed-length-of-sixty-three-characters"},
			wantErr:  true,
			errorMsg: "must be between 3 and 63 characters",
		},
		{
			name:    "no buckets specified",
			buckets: []string{},
			wantErr: false,
		},
		{
			name:    "nil buckets",
			buckets: nil,
			wantErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Storage.Buckets = tc.buckets

			err := cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLoadConfig_WithBuckets(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
server:
  addr: ":8080"

storage:
  root: "/data"
  type: "filesystem"
  buckets:
    - bucket1
    - bucket2
    - bucket3

observability:
  serviceName: "test"
`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	assert.Equal(t, []string{"bucket1", "bucket2", "bucket3"}, cfg.Storage.Buckets)
}

func TestSaveConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "test-config.yaml")

	cfg := Config{
		Server: ServerConfig{
			Addr:         ":9000",
			ReadTimeout:  45 * time.Second,
			WriteTimeout: 60 * time.Second,
			IdleTimeout:  150 * time.Second,
			HealthPath:   "/healthz",
		},
		Storage: StorageConfig{
			Root: "/tmp/s3",
			Type: "filesystem",
		},
		Observability: ObservabilityConfig{
			ServiceName:          "test-service",
			EnableRequestLogging: false,
			EnableMetrics:        true,
			EnableTracing:        true,
		},
	}

	// Save config
	err := SaveConfig(cfg, configPath)
	require.NoError(t, err)

	// Verify file exists
	_, err = os.Stat(configPath)
	require.NoError(t, err)

	// Load it back and verify
	loadedCfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	assert.Equal(t, cfg.Server.Addr, loadedCfg.Server.Addr)
	assert.Equal(t, cfg.Server.ReadTimeout, loadedCfg.Server.ReadTimeout)
	assert.Equal(t, cfg.Storage.Root, loadedCfg.Storage.Root)
	assert.Equal(t, cfg.Observability.ServiceName, loadedCfg.Observability.ServiceName)
	assert.Equal(t, cfg.Observability.EnableRequestLogging, loadedCfg.Observability.EnableRequestLogging)
}

func TestSaveConfig_InvalidPath(t *testing.T) {
	cfg := DefaultConfig()

	// Try to save to an invalid path (directory that doesn't exist)
	err := SaveConfig(cfg, "/nonexistent/directory/config.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write config file")
}
