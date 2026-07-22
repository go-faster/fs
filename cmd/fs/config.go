package main

import (
	"fmt"
	"os"
	"time"

	"github.com/go-faster/errors"
	"gopkg.in/yaml.v3"

	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/validate"
	"github.com/go-faster/fs/server"
)

// StorageTypeFilesystem is the single-node filesystem storage backend.
const StorageTypeFilesystem = "filesystem"

// StorageTypeCluster is the replicated cluster storage backend (M3): objects
// are placed across the nodes registered in etcd, written at quorum and
// served by any node.
const StorageTypeCluster = "cluster"

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

	// Cluster configuration (used when storage.type is "cluster")
	Cluster ClusterConfig `yaml:"cluster,omitempty"`

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

// DefaultClusterAddr is the default cluster (peer replication) listener
// address.
const DefaultClusterAddr = ":7080"

// ClusterConfig configures cluster mode: this node's identity and disks, the
// shared peer-auth secret, the replication scheme and the etcd control plane.
type ClusterConfig struct {
	// NodeID uniquely identifies this node in the cluster. Required.
	NodeID string `yaml:"node_id"`

	// Rack is this node's failure-domain label (nodes sharing a rack share
	// fate); placement spreads copies across racks first. Empty means the node
	// is its own failure domain.
	Rack string `yaml:"rack,omitempty"`

	// Addr is the cluster listener bind address for the peer replication API
	// (default DefaultClusterAddr). Internal — never expose it publicly; peers
	// authenticate with the cluster secret.
	Addr string `yaml:"addr,omitempty"`

	// AdvertiseAddr is the host:port peers dial to reach this node's cluster
	// listener. Required (a bind address like ":7080" is not dialable).
	AdvertiseAddr string `yaml:"advertise_addr"`

	// Secret is the shared cluster secret authenticating peer traffic (HMAC,
	// mutual). Required, min 16 characters; the FS_CLUSTER_SECRET environment
	// variable takes precedence.
	Secret string `yaml:"secret,omitempty"`

	// Scheme is the default replication scheme for all buckets: "rf2.5"
	// (default), "rf3" or "ec:k,m" (e.g. "ec:4,2").
	Scheme string `yaml:"scheme,omitempty"`

	// Disks are this node's storage devices. Default: a single disk "d0"
	// under <storage.root>/cluster/d0 with weight 1.
	Disks []ClusterDiskConfig `yaml:"disks,omitempty"`

	// Etcd configures the control plane connection.
	Etcd EtcdConfig `yaml:"etcd"`
}

// ClusterDiskConfig is one local disk exposed to the cluster.
type ClusterDiskConfig struct {
	// ID identifies the disk within this node. Required.
	ID string `yaml:"id"`
	// Path is the disk's root directory. Required.
	Path string `yaml:"path"`
	// Weight is the relative capacity weight for placement (default 1; 0 or
	// negative drains the disk — no new data placed on it).
	Weight float64 `yaml:"weight,omitempty"`
}

// EtcdConfig configures the etcd control-plane connection.
type EtcdConfig struct {
	// Endpoints are the etcd client URLs. Required in cluster mode.
	Endpoints []string `yaml:"endpoints"`
	// Prefix namespaces this cluster's keys (default "/fs").
	Prefix string `yaml:"prefix,omitempty"`
	// TTL is the node registration lease: how long a dead node lingers in the
	// topology (default 10s, minimum 1s).
	TTL time.Duration `yaml:"ttl,omitempty"`
}

// ClusterSecret resolves the effective cluster secret (FS_CLUSTER_SECRET
// overrides the config value).
func (c *Config) ClusterSecret() string {
	if env := os.Getenv("FS_CLUSTER_SECRET"); env != "" {
		return env
	}

	return c.Cluster.Secret
}

// validateCluster checks the cluster section (called when storage.type is
// "cluster").
func (c *Config) validateCluster() error {
	cc := c.Cluster

	if cc.NodeID == "" {
		return errors.New("cluster.node_id is required")
	}

	if cc.AdvertiseAddr == "" {
		return errors.New("cluster.advertise_addr is required (peers must be able to dial this node)")
	}

	if len(c.ClusterSecret()) < 16 {
		return errors.New("cluster.secret (or FS_CLUSTER_SECRET) is required, min 16 characters")
	}

	if cc.Scheme != "" {
		if _, err := scheme.Parse(cc.Scheme); err != nil {
			return errors.Wrap(err, "cluster.scheme")
		}
	}

	if len(cc.Etcd.Endpoints) == 0 {
		return errors.New("cluster.etcd.endpoints is required")
	}

	if cc.Etcd.TTL != 0 && cc.Etcd.TTL < time.Second {
		return errors.New("cluster.etcd.ttl must be at least 1s")
	}

	seen := make(map[string]struct{}, len(cc.Disks))

	for i, d := range cc.Disks {
		if d.ID == "" || d.Path == "" {
			return fmt.Errorf("cluster.disks[%d]: id and path are required", i)
		}

		if _, dup := seen[d.ID]; dup {
			return fmt.Errorf("cluster.disks[%d]: duplicate disk id %q", i, d.ID)
		}

		seen[d.ID] = struct{}{}
	}

	return nil
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

	switch c.Storage.Type {
	case StorageTypeFilesystem:
	case StorageTypeCluster:
		if err := c.validateCluster(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported storage type: %s (want %q or %q)", c.Storage.Type, StorageTypeFilesystem, StorageTypeCluster)
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
