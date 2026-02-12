# Configuration Guide

go-faster/fs supports flexible YAML-based configuration management, with command-line flags for quick overrides.

## Table of Contents

- [Quick Start](#quick-start)
- [Configuration Options](#configuration-options)
- [Configuration Examples](#configuration-examples)
- [Environment-Specific Configurations](#environment-specific-configurations)
- [Command-Line Overrides](#command-line-overrides)
- [Generating Configuration Files](#generating-configuration-files)

## Quick Start

### Using Defaults

Start the server with default configuration:

```bash
fs s3
```

This uses:
- Address: `:8080`
- Storage root: `.s3data`
- All default timeouts and observability settings

### Using a Configuration File

Start with a YAML configuration file:

```bash
fs s3 --config config.yaml
```

### Override with Flags

Override specific values from the config file:

```bash
fs s3 --config config.yaml --addr :9000 --root /data/s3
```

## Configuration Options

### Complete Configuration Reference

```yaml
# Server configuration
server:
  # Address to listen on (e.g., ":8080", "127.0.0.1:8080", "0.0.0.0:9000")
  addr: ":8080"

  # HTTP server timeouts
  read_timeout: 30s      # Maximum duration for reading request
  write_timeout: 30s     # Maximum duration for writing response
  idle_timeout: 120s     # Maximum idle time between requests

  # Health check endpoint path
  health_path: "/health"

# Storage configuration
storage:
  # Root directory for S3 storage
  root: ".s3data"

  # Storage backend type (currently only "filesystem" is supported)
  type: "filesystem"

  # Pre-create these buckets on startup (optional)
  buckets:
    - my-bucket
    - uploads
    - backups

# Observability configuration
observability:
  # Service name for telemetry
  service_name: "go-faster/fs"

  # Enable HTTP request logging
  enable_request_logging: true

  # Enable Prometheus metrics
  enable_metrics: true

  # Enable OpenTelemetry tracing
  enable_tracing: true
```

### Server Configuration

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `server.addr` | string | `:8080` | Network address to listen on |
| `server.read_timeout` | duration | `30s` | Maximum time to read request |
| `server.write_timeout` | duration | `30s` | Maximum time to write response |
| `server.idle_timeout` | duration | `120s` | Maximum idle time between requests |
| `server.health_path` | string | `/health` | Health check endpoint path |

### Storage Configuration

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `storage.root` | string | `.s3data` | Root directory for object storage |
| `storage.type` | string | `filesystem` | Storage backend type |
| `storage.buckets` | []string | `[]` | Buckets to pre-create on startup |

### Observability Configuration

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `observability.service_name` | string | `go-faster/fs` | Service name for telemetry |
| `observability.enable_request_logging` | bool | `true` | Enable HTTP request logging |
| `observability.enable_metrics` | bool | `true` | Enable Prometheus metrics |
| `observability.enable_tracing` | bool | `true` | Enable OpenTelemetry tracing |

## Configuration Examples

### Minimal Configuration

```yaml
server:
  addr: ":8080"
storage:
  root: "/data/s3"
observability:
  service_name: "my-s3-server"
```

### Development Configuration

```yaml
server:
  addr: "127.0.0.1:8080"  # Localhost only
  read_timeout: 10s
  write_timeout: 10s
  idle_timeout: 30s

storage:
  root: ".s3data"

observability:
  service_name: "go-faster/fs-dev"
  enable_request_logging: true
  enable_metrics: true
  enable_tracing: false  # Disable tracing in dev
```

### Production Configuration

```yaml
server:
  addr: ":8080"           # All interfaces
  read_timeout: 60s
  write_timeout: 120s     # Longer for large uploads
  idle_timeout: 300s      # 5 minutes
  health_path: "/health"

storage:
  root: "/data/s3"        # Persistent volume mount
  type: "filesystem"

observability:
  service_name: "go-faster/fs"
  enable_request_logging: true
  enable_metrics: true
  enable_tracing: true
```

### High-Performance Configuration

```yaml
server:
  addr: ":8080"
  read_timeout: 120s
  write_timeout: 300s     # 5 minutes for large files
  idle_timeout: 600s      # 10 minutes

storage:
  root: "/mnt/fast-ssd/s3"

observability:
  service_name: "go-faster/fs-prod"
  enable_request_logging: false  # Disable to reduce overhead
  enable_metrics: true
  enable_tracing: false
```

### Bucket Pre-Creation

You can configure buckets to be automatically created when the server starts:

```yaml
storage:
  root: "/data/s3"
  type: "filesystem"
  buckets:
    - uploads
    - backups
    - temp-files
```

**Benefits:**
- Ensures required buckets exist before applications connect
- Useful for Kubernetes deployments with init requirements
- Simplifies setup in development and testing environments

**Behavior:**
- Buckets are created during server startup
- If a bucket already exists, it's skipped (no error)
- Failed bucket creation will prevent server startup
- Bucket names are validated (3-63 characters)

**Example log output:**
```
INFO    Pre-creating buckets    {"buckets": ["uploads", "backups", "temp-files"]}
INFO    Created bucket  {"bucket": "uploads"}
INFO    Created bucket  {"bucket": "backups"}
INFO    Bucket already exists, skipping    {"bucket": "temp-files"}
```

## Environment-Specific Configurations

The project includes pre-configured files for different environments:

### `config.yaml` - Default Configuration
Balanced settings suitable for general use.

```bash
fs s3 --config config.yaml
```

### `config.dev.yaml` - Development
- Binds to localhost only
- Shorter timeouts for faster feedback
- Verbose logging enabled

```bash
fs s3 --config config.dev.yaml
```

### `config.production.yaml` - Production
- Longer timeouts for large files
- All observability features enabled
- Production-ready paths

```bash
fs s3 --config config.production.yaml
```

## Command-Line Overrides

Command-line flags take precedence over configuration file values:

### Available Flags

| Flag | Short | Type | Description |
|------|-------|------|-------------|
| `--config` | `-c` | string | Path to YAML configuration file |
| `--addr` | | string | Address to listen on |
| `--root` | | string | Root directory for storage |
| `--generate-config` | | bool | Generate example config and exit |

### Override Examples

Start with config file but change port:
```bash
fs s3 --config config.yaml --addr :9000
```

Use custom storage directory:
```bash
fs s3 --config config.yaml --root /mnt/storage/s3
```

Override both address and storage:
```bash
fs s3 --config config.yaml --addr :9000 --root /data/s3
```

Run without any config file (pure flags):
```bash
fs s3 --addr :9000 --root /var/lib/s3data
```

## Generating Configuration Files

Generate a complete example configuration:

```bash
fs s3 --generate-config > my-config.yaml
```

This creates a file with all available options and their default values.

You can then edit this file to customize your configuration:

```bash
# Generate base config
fs s3 --generate-config > config.yaml

# Edit the file
vim config.yaml

# Start with your custom config
fs s3 --config config.yaml
```

## Configuration Priority

Configuration values are resolved in the following order (highest to lowest priority):

1. **Command-line flags** (e.g., `--addr :9000`)
2. **Configuration file** (e.g., `config.yaml`)
3. **Default values** (built-in defaults)

Example:
```bash
# If config.yaml has addr: ":8080"
# But you run: fs s3 --config config.yaml --addr :9000
# The server will listen on :9000 (flag overrides file)
```

## Validation

The configuration is validated on startup. The server will not start if:

- `server.addr` is empty
- `storage.root` is empty
- `storage.type` is not "filesystem"
- Any timeout value is <= 0
- `observability.service_name` is empty

Error example:
```bash
$ fs s3 --addr ""
Error validating config: server.addr is required
```

## Duration Format

Timeout values use Go's duration syntax:

- `30s` - 30 seconds
- `5m` - 5 minutes
- `1h30m` - 1 hour 30 minutes
- `2h` - 2 hours

Examples:
```yaml
server:
  read_timeout: 30s      # 30 seconds
  write_timeout: 2m      # 2 minutes
  idle_timeout: 5m30s    # 5 minutes 30 seconds
```

## Troubleshooting

### Config file not found
If the config file doesn't exist and you specify `--config`, the server will use default values:

```bash
# Even if missing-config.yaml doesn't exist, this works (uses defaults)
fs s3 --config missing-config.yaml
```

### Invalid YAML syntax
```bash
Error loading config: parse config: yaml: line 5: ...
```
Check your YAML syntax. Common issues:
- Incorrect indentation (use 2 or 4 spaces, not tabs)
- Missing colons after keys
- Unquoted special characters

### Invalid duration format
```bash
Error validating config: invalid duration ...
```
Use valid duration format: `30s`, `5m`, `1h`, etc.

### Permission denied
```bash
Error: failed to create storage: mkdir /data/s3: permission denied
```
Ensure the user running the server has write permissions to the storage directory.

## Best Practices

1. **Use configuration files for deployments**
   - Easier to version control
   - More maintainable than long command lines
   - Can document choices with comments

2. **Use flags for quick testing**
   - Fast iteration during development
   - Temporary overrides

3. **Keep environment-specific configs**
   - `config.dev.yaml` for development
   - `config.staging.yaml` for staging
   - `config.production.yaml` for production

4. **Version control your configs**
   ```bash
   git add config*.yaml
   ```

5. **Don't commit secrets**
   - If you add authentication in the future
   - Use environment variables or external secret management

6. **Document custom configurations**
   - Add comments explaining non-obvious choices
   - Note any dependencies or requirements

## Examples in Practice

### Docker Deployment

```dockerfile
FROM alpine:latest
COPY fs /usr/local/bin/
COPY config.production.yaml /etc/fs/config.yaml

CMD ["fs", "s3", "--config", "/etc/fs/config.yaml"]
```

### Kubernetes ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: fs-config
data:
  config.yaml: |
    server:
      addr: ":8080"
      read_timeout: 60s
      write_timeout: 120s
    storage:
      root: "/data/s3"
    observability:
      service_name: "go-faster/fs-prod"
```

### systemd Service

```ini
[Unit]
Description=go-faster/fs S3 Storage Server
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/fs s3 --config /etc/fs/config.yaml
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Additional Resources

- [README.md](README.md) - Main project documentation
- [FEATURES.md](FEATURES.md) - Feature implementation status
- [CONTRIBUTING.md](CONTRIBUTING.md) - Contributing guidelines

