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

	// Auth configuration
	Auth AuthConfig `yaml:"auth"`

	// Admin configuration
	Admin AdminConfig `yaml:"admin,omitempty"`

	// Integrity configuration
	Integrity IntegrityConfig `yaml:"integrity"`

	// Observability configuration
	Observability ObservabilityConfig `yaml:"observability"`
}

// IntegrityConfig configures object integrity checking.
type IntegrityConfig struct {
	// VerifyOnRead recomputes and checks each object's checksum before serving
	// it (costs a full extra read per GET). Off by default.
	VerifyOnRead bool `yaml:"verify_on_read,omitempty"`

	// ScrubInterval, if positive, runs a background scrubber that walks all
	// objects on this cadence and reports bit-rot. Zero disables it.
	ScrubInterval time.Duration `yaml:"scrub_interval,omitempty"`

	// ScrubQuarantine moves corrupt objects aside (into <root>/.quarantine)
	// instead of only reporting them.
	ScrubQuarantine bool `yaml:"scrub_quarantine,omitempty"`
}

// AuthConfig configures authentication and authorization.
type AuthConfig struct {
	// Disabled turns off authentication entirely (anonymous access). Equivalent
	// to the --insecure-no-auth flag.
	Disabled bool `yaml:"disabled,omitempty"`

	// Keys are the credentials the server accepts. A root credential can also be
	// supplied via the FS_ROOT_ACCESS_KEY / FS_ROOT_SECRET_KEY environment
	// variables (granted admin on all buckets).
	Keys []KeyConfig `yaml:"keys,omitempty"`

	// PublicReadBuckets may be read anonymously.
	PublicReadBuckets []string `yaml:"public_read_buckets,omitempty"`
}

// DefaultAdminAddr is the default admin listener address.
const DefaultAdminAddr = "localhost:8090"

// DefaultAdminKeysFile is the default filename (under the storage root) for
// persisted runtime-created access keys.
const DefaultAdminKeysFile = ".access-keys.json"

// AdminConfig configures the admin API and its embedded web dashboard, served
// on a separate listener protected by a bearer token.
type AdminConfig struct {
	// Enabled turns on the admin listener. Off by default.
	Enabled bool `yaml:"enabled,omitempty"`

	// Addr is the admin listener address. Defaults to localhost:8090; keep it
	// bound to localhost or behind a proxy — it manages credentials.
	Addr string `yaml:"addr,omitempty"`

	// Token is the bearer token required on every admin API request. It may also
	// be supplied via the FS_ADMIN_TOKEN environment variable, which takes
	// precedence. Required when Enabled.
	Token string `yaml:"token,omitempty"`

	// KeysFile persists runtime-created access keys. Defaults to
	// <storage.root>/.access-keys.json.
	KeysFile string `yaml:"keys_file,omitempty"`
}

// KeyConfig is one credential and its grants.
type KeyConfig struct {
	AccessKey string        `yaml:"access_key"`
	SecretKey string        `yaml:"secret_key"`
	Grants    []GrantConfig `yaml:"grants,omitempty"`
}

// GrantConfig authorizes an access key for buckets matching Bucket (a glob) up
// to Permission ("read", "write" or "admin").
type GrantConfig struct {
	Bucket     string `yaml:"bucket"`
	Permission string `yaml:"permission"`
}

// TLSConfig configures TLS termination.
type TLSConfig struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
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

	// TLS, if both files are set, serves HTTPS with hot-reloadable certificates.
	TLS TLSConfig `yaml:"tls,omitempty"`
}

// StorageConfig contains storage backend configuration.
type StorageConfig struct {
	// Root directory for S3 storage
	Root string `yaml:"root"`

	// Type of storage backend (currently only "filesystem" is supported)
	Type string `yaml:"type"`

	// Fsync is the durability policy: "none", "file" or "file+dir". The binary
	// defaults to "file"; set "none" for dev/CI to trade durability for speed.
	Fsync string `yaml:"fsync,omitempty"`

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
			Root:  DefaultStorageRoot,
			Type:  StorageTypeFilesystem,
			Fsync: "file",
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
