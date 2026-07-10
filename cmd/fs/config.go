package main

import (
	"fmt"
	"os"
	"time"

	"github.com/go-faster/errors"
	"gopkg.in/yaml.v3"

	"github.com/go-faster/fs/internal/validate"
	"github.com/go-faster/fs/server"
)

// StorageTypeFilesystem is the only currently supported storage backend type.
const StorageTypeFilesystem = "filesystem"

// DefaultStorageRoot is the default directory for filesystem storage.
const DefaultStorageRoot = ".s3data"

// Config represents the application configuration.
type Config struct {
	// Server configuration
	Server ServerConfig `yaml:"server"`

	// Storage configuration
	Storage StorageConfig `yaml:"storage"`

	// Observability configuration
	Observability ObservabilityConfig `yaml:"observability"`
}

// ServerConfig contains HTTP server configuration.
type ServerConfig struct {
	// Address to listen on (e.g., ":8080", "127.0.0.1:8080")
	Addr string `yaml:"addr"`

	// ReadTimeout is the maximum duration for reading the entire request
	ReadTimeout time.Duration `yaml:"read_timeout"`

	// WriteTimeout is the maximum duration before timing out writes of the response
	WriteTimeout time.Duration `yaml:"write_timeout"`

	// IdleTimeout is the maximum amount of time to wait for the next request
	IdleTimeout time.Duration `yaml:"idle_timeout"`

	// HealthPath is the path for health check endpoint
	HealthPath string `yaml:"health_path"`
}

// StorageConfig contains storage backend configuration.
type StorageConfig struct {
	// Root directory for S3 storage
	Root string `yaml:"root"`

	// Type of storage backend (currently only "filesystem" is supported)
	Type string `yaml:"type"`

	// Buckets to pre-create on startup (optional)
	Buckets []string `yaml:"buckets,omitempty"`
}

// ObservabilityConfig contains telemetry and observability settings.
type ObservabilityConfig struct {
	// ServiceName for telemetry
	ServiceName string `yaml:"service_name"`

	// EnableRequestLogging enables HTTP request logging
	EnableRequestLogging bool `yaml:"enable_request_logging"`

	// EnableMetrics enables Prometheus metrics
	EnableMetrics bool `yaml:"enable_metrics"`

	// EnableTracing enables OpenTelemetry tracing
	EnableTracing bool `yaml:"enable_tracing"`
}

// DefaultConfig returns a configuration with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Addr:         server.DefaultAddr,
			ReadTimeout:  server.DefaultReadTimeout,
			WriteTimeout: server.DefaultWriteTimeout,
			IdleTimeout:  server.DefaultIdleTimeout,
			HealthPath:   server.DefaultHealthPath,
		},
		Storage: StorageConfig{
			Root: DefaultStorageRoot,
			Type: StorageTypeFilesystem,
		},
		Observability: ObservabilityConfig{
			ServiceName:          "go-faster/fs",
			EnableRequestLogging: true,
			EnableMetrics:        true,
			EnableTracing:        true,
		},
	}
}

// LoadConfig loads configuration from a YAML file.
// If the file doesn't exist or path is empty, returns default configuration.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	// If no path provided, return defaults
	if path == "" {
		return cfg, nil
	}

	// Read the file
	data, err := os.ReadFile(path) // #nosec G304 -- config files
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}

		return Config{}, errors.Wrap(err, "read config file")
	}

	// Parse YAML
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, errors.Wrap(err, "parse config")
	}

	return cfg, nil
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return errors.New("server.addr is required")
	}

	if c.Storage.Root == "" {
		return errors.New("storage.root is required")
	}

	if c.Storage.Type != StorageTypeFilesystem {
		return fmt.Errorf("unsupported storage type: %s (only 'filesystem' is supported)", c.Storage.Type)
	}

	if c.Server.ReadTimeout <= 0 {
		return errors.New("server.read_timeout must be positive")
	}

	if c.Server.WriteTimeout <= 0 {
		return errors.New("server.write_timeout must be positive")
	}

	if c.Server.IdleTimeout <= 0 {
		return errors.New("server.idle_timeout must be positive")
	}

	if c.Observability.ServiceName == "" {
		return errors.New("observability.service_name is required")
	}

	// Validate bucket names with the same rules the server enforces at runtime.
	for _, bucket := range c.Storage.Buckets {
		if err := validate.BucketName(bucket); err != nil {
			return errors.Wrapf(err, "invalid bucket name %q", bucket)
		}
	}

	return nil
}

// SaveConfig saves the configuration to a YAML file.
func SaveConfig(cfg Config, path string) error {
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return errors.Wrap(err, "marshal config")
	}

	// #nosec G306 -- config files
	if err := os.WriteFile(path, data, 0644); err != nil {
		return errors.Wrap(err, "write config file")
	}

	return nil
}
