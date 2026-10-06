package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/go-faster/errors"
	"gopkg.in/yaml.v3"

	"github.com/go-faster/fs/internal/validate"
	"github.com/go-faster/fs/server"
)

// storage.fsync values.
const (
	fsyncFile = "file"
	fsyncNone = "none"
)

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

	// Cluster configures cluster membership; setting node_id turns it on.
	Cluster ClusterConfig `yaml:"cluster,omitempty"`

	// Lifecycle configures enforcement of bucket lifecycle rules.
	Lifecycle LifecycleConfig `yaml:"lifecycle,omitempty"`

	// Encryption configures server-side encryption of object bodies at rest.
	Encryption EncryptionConfig `yaml:"encryption,omitempty"`

	// Observability configuration
	Observability ObservabilityConfig `yaml:"observability"`

	// Revision is an opaque marker for orchestrators. fs never acts on it; it
	// only echoes the value through the admin API — InstanceInfo.config_revision
	// and the reload result — and refreshes it on reload. A controller that
	// renders configs can stamp each one with a revision and read it back to
	// confirm which config a node has actually loaded (e.g. after a hot
	// reload), without shelling into the process.
	Revision string `yaml:"revision,omitempty"`
}

// DefaultLifecycleInterval is how often lifecycle rules are enforced. Expiry is
// eventual by design — S3 promises the object goes away, not when — so the pass
// is spaced to cost little rather than to be prompt.
const DefaultLifecycleInterval = 12 * time.Hour

// LifecycleConfig configures the background sweep that enforces bucket
// lifecycle rules.
type LifecycleConfig struct {
	// Interval is how often the sweep runs. Zero disables enforcement, which
	// leaves any rule a client sets stored but inert; the server says so
	// loudly at startup, because a lifecycle rule nobody enforces is a client
	// told its data expires when nothing will delete it.
	Interval time.Duration `yaml:"interval,omitempty"`
}

// AuthConfig configures authentication and authorization.
type AuthConfig struct {
	// Disabled turns off authentication entirely (anonymous access). Equivalent
	// to the --insecure-no-auth flag.
	Disabled bool `yaml:"disabled,omitempty"`

	// OwnerIsolation makes a bucket reachable only by the principal that
	// created it, plus any credential whose grant names the bucket rather than
	// matching it through a wildcard. Off by default: turning it on changes
	// what an existing deployment's "*" grants mean, so it is the operator's
	// call and not an upgrade's side effect. Ownership is recorded either way.
	OwnerIsolation bool `yaml:"owner_isolation,omitempty"`

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

// AdminConfig configures the admin API, served on a separate listener
// protected by a bearer token.
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

// KeyConfig is one credential, the owner identity it acts as, and its grants.
type KeyConfig struct {
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// UserID is the canonical owner ID reported in <Owner> elements for
	// objects this key writes. Defaults to the access key.
	UserID string `yaml:"user_id,omitempty"`
	// DisplayName is the human-readable owner name. Defaults to UserID.
	DisplayName string        `yaml:"display_name,omitempty"`
	Grants      []GrantConfig `yaml:"grants,omitempty"`
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

	// Region names the location reported for buckets (GetBucketLocation).
	// Empty is the S3 default region, reported as an empty constraint.
	Region string `yaml:"region,omitempty"`

	// TLS, if both files are set, serves HTTPS with hot-reloadable certificates.
	TLS TLSConfig `yaml:"tls,omitempty"`
}

// StorageConfig contains storage backend configuration.
type StorageConfig struct {
	// Root directory for S3 storage
	Root string `yaml:"root"`

	// Fsync is "file" (the default: an acknowledged write is on disk) or
	// "none" (no fsync at all — for dev and CI, where a crash may lose
	// acknowledged writes).
	Fsync string `yaml:"fsync,omitempty"`

	// Buckets to pre-create on startup (optional)
	Buckets []string `yaml:"buckets,omitempty"`

	// Background sets how often the engine's background work runs. Zero
	// values keep the defaults; there is rarely a reason to change them
	// outside tests.
	Background BackgroundConfig `yaml:"background,omitempty"`
}

// BackgroundConfig tunes the engine's background work (engine.RunConfig).
type BackgroundConfig struct {
	// SyncInterval is the anti-entropy period (default 10m).
	SyncInterval time.Duration `yaml:"sync_interval,omitempty"`
	// ResyncInterval is how often queued block copies are retried
	// (default 10s).
	ResyncInterval time.Duration `yaml:"resync_interval,omitempty"`
	// GCInterval is the collection period (default 1h), GCGrace how long an
	// unreferenced block is kept (default 10m), and TombstoneDelay how long
	// a deleted row is kept before it is collected (default 24h).
	GCInterval     time.Duration `yaml:"gc_interval,omitempty"`
	GCGrace        time.Duration `yaml:"gc_grace,omitempty"`
	TombstoneDelay time.Duration `yaml:"tombstone_delay,omitempty"`
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
			Fsync: fsyncFile,
		},
		Lifecycle: LifecycleConfig{
			Interval: DefaultLifecycleInterval,
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

	// Strict: a key this binary does not know — misspelled, or a setting
	// a release removed — stops the server rather than being ignored.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, errors.Wrap(err, "parse config")
	}

	cfg.Encryption.resolvePaths(filepath.Dir(path))

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

	if err := c.validateCluster(); err != nil {
		return err
	}

	switch c.Storage.Fsync {
	case "", fsyncNone, fsyncFile:
	default:
		return fmt.Errorf("invalid storage.fsync %q (want \"file\" or \"none\")", c.Storage.Fsync)
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

	if err := c.Encryption.Validate(); err != nil {
		return err
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
