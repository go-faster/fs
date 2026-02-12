# YAML Configuration Quick Reference

## TL;DR

```bash
# Generate config template
fs s3 --generate-config > config.yaml

# Use config file
fs s3 --config config.yaml

# Override specific values
fs s3 --config config.yaml --addr :9000
```

## Configuration File Structure

```yaml
server:
  addr: ":8080"              # Listen address
  read_timeout: "30s"        # Request read timeout
  write_timeout: "30s"       # Response write timeout
  idle_timeout: "2m0s"       # Idle connection timeout
  health_path: "/health"     # Health check endpoint

storage:
  root: ".s3data"            # Storage directory
  type: "filesystem"         # Backend type
  buckets:                   # Pre-create buckets (optional)
    - my-bucket
    - uploads

observability:
  service_name: "go-faster/fs"          # Service name for telemetry
  enable_request_logging: true           # HTTP request logs
  enable_metrics: true                   # Prometheus metrics
  enable_tracing: true                   # OpenTelemetry traces
```

## Quick Examples

### Development
```yaml
server:
  addr: "127.0.0.1:8080"
  read_timeout: "10s"
  write_timeout: "10s"
  idle_timeout: "30s"
```

### Production
```yaml
server:
  addr: ":8080"
  read_timeout: "60s"
  write_timeout: "2m0s"
  idle_timeout: "5m0s"
```

### High Performance (Large Files)
```yaml
server:
  write_timeout: "5m0s"
  idle_timeout: "10m0s"
```

## Helm Quick Reference

```yaml
# values.yaml
config:
  server:
    addr: ":8080"
    readTimeout: "30s"
    writeTimeout: "30s"
    idleTimeout: "2m0s"
  storage:
    root: "/data"
  observability:
    serviceName: "my-s3"
```

```bash
# Deploy with custom config
helm install my-s3 ./go-faster-fs -f values.yaml

# View generated ConfigMap
kubectl get cm <release>-go-faster-fs-config -o yaml
```

## Common Patterns

### Pre-Create Buckets
```yaml
storage:
  root: "/data"
  buckets:
    - uploads
    - backups
    - temp-files
```

### Disable Request Logging (Performance)
```yaml
observability:
  enable_request_logging: false
```

### Custom Storage Location
```yaml
storage:
  root: "/mnt/fast-ssd/s3data"
```

### External Monitoring Only
```yaml
observability:
  enable_request_logging: false
  enable_metrics: true
  enable_tracing: true
```

## Duration Format

- `30s` = 30 seconds
- `5m` = 5 minutes
- `2h` = 2 hours
- `1h30m` = 1.5 hours
- `90s` = 1 minute 30 seconds

## CLI Override Priority

```
Command-line flags > Config file > Defaults
```

Example:
```bash
# addr will be :9000 (flag overrides config)
fs s3 --config config.yaml --addr :9000
```

## Validation

Configuration is validated on startup. Common errors:

- Empty required fields (addr, root, service_name)
- Invalid duration format
- Negative or zero timeouts
- Invalid storage type (must be "filesystem")

## Files Reference

- `config.yaml` - Default configuration
- `config.dev.yaml` - Development settings
- `config.production.yaml` - Production settings
- `CONFIGURATION.md` - Full documentation

## Need Help?

```bash
# Show help
fs s3 --help

# Generate example config
fs s3 --generate-config

# Validate by running (will error if invalid)
fs s3 --config myconfig.yaml
```

