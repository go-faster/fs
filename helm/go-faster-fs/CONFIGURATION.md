# Helm Chart Configuration Guide

This guide describes how to configure the go-faster/fs Helm chart.

## Overview

The Helm chart uses a ConfigMap to manage the application configuration. All configuration is defined in the `config` section of `values.yaml` and is automatically rendered as a YAML configuration file mounted into the pod.

## Configuration Structure

The configuration is divided into three main sections:

### Server Configuration

Controls HTTP server behavior:

```yaml
config:
  server:
    addr: ":8080"           # Listen address
    readTimeout: "30s"      # Request read timeout
    writeTimeout: "30s"     # Response write timeout
    idleTimeout: "2m0s"     # Connection idle timeout
    healthPath: "/health"   # Health check endpoint
```

### Storage Configuration

Controls where and how data is stored:

```yaml
config:
  storage:
    root: "/data"           # Storage root directory
    type: "filesystem"      # Storage backend (only filesystem supported)
```

### Observability Configuration

Controls telemetry and monitoring:

```yaml
config:
  observability:
    serviceName: "go-faster/fs"          # Service name for traces/metrics
    enableRequestLogging: true            # Enable HTTP request logging
    enableMetrics: true                   # Enable Prometheus metrics
    enableTracing: true                   # Enable OpenTelemetry tracing
```

## How It Works

1. The configuration in `values.yaml` is rendered into a ConfigMap
2. The ConfigMap is mounted at `/etc/fs/config.yaml` in the pod
3. The application is started with `--config=/etc/fs/config.yaml`
4. Configuration changes require a pod restart to take effect

## Example: Custom Configuration

Create a custom `values.yaml`:

```yaml
config:
  server:
    addr: ":9000"
    readTimeout: "60s"
    writeTimeout: "120s"
    idleTimeout: "5m0s"

  storage:
    root: "/mnt/s3-data"

  observability:
    serviceName: "my-s3-server"
    enableRequestLogging: true
    enableMetrics: true
    enableTracing: false
```

Deploy with:

```bash
helm install my-s3 . -f custom-values.yaml
```

## Production Configuration

The chart includes a production-ready configuration in `values-production.yaml`:

```bash
helm install my-s3 . -f values-production.yaml
```

Key production settings:
- Increased timeouts for large file operations
- Persistent volume for data storage
- Resource limits and requests
- Horizontal pod autoscaling
- Ingress configuration
- Pod anti-affinity

## Updating Configuration

To update the configuration:

```bash
# Edit your values file
vim my-values.yaml

# Upgrade the release
helm upgrade my-s3 . -f my-values.yaml
```

The ConfigMap will be updated and pods will be restarted automatically if the ConfigMap content changes.

## ConfigMap Content

You can view the generated ConfigMap:

```bash
# View the ConfigMap YAML
kubectl get configmap <release-name>-go-faster-fs-config -o yaml

# View just the config.yaml content
kubectl get configmap <release-name>-go-faster-fs-config -o jsonpath='{.data.config\.yaml}'
```

## Duration Format

Timeout values use Go's duration format:
- `30s` - 30 seconds
- `2m` - 2 minutes
- `5m30s` - 5 minutes 30 seconds
- `1h` - 1 hour
- `1h30m` - 1 hour 30 minutes

## Environment-Specific Configuration

### Development

```yaml
config:
  server:
    readTimeout: "10s"
    writeTimeout: "10s"
    idleTimeout: "30s"
  observability:
    serviceName: "go-faster/fs-dev"
    enableRequestLogging: true
```

### Staging

```yaml
config:
  server:
    readTimeout: "30s"
    writeTimeout: "60s"
    idleTimeout: "2m0s"
  observability:
    serviceName: "go-faster/fs-staging"
```

### Production

```yaml
config:
  server:
    readTimeout: "60s"
    writeTimeout: "2m0s"   # For large uploads
    idleTimeout: "5m0s"
  observability:
    serviceName: "go-faster/fs-prod"
    enableRequestLogging: true
    enableMetrics: true
    enableTracing: true
```

## Troubleshooting

### Configuration Not Applied

If configuration changes aren't taking effect:

1. Check the ConfigMap was updated:
   ```bash
   kubectl get configmap <release-name>-go-faster-fs-config -o yaml
   ```

2. Check if pods picked up the change:
   ```bash
   kubectl rollout status deployment <release-name>-go-faster-fs
   ```

3. Force a rollout restart:
   ```bash
   kubectl rollout restart deployment <release-name>-go-faster-fs
   ```

### Invalid Configuration

If the pod fails to start due to invalid configuration:

1. Check pod logs:
   ```bash
   kubectl logs deployment/<release-name>-go-faster-fs
   ```

2. Common issues:
   - Invalid duration format (must be like `30s`, `2m`)
   - Missing required fields
   - Invalid storage.type (must be "filesystem")

### View Running Configuration

To see what configuration is actually running:

```bash
# Get the config from the mounted ConfigMap
kubectl exec deployment/<release-name>-go-faster-fs -- cat /etc/fs/config.yaml
```

## Advanced Usage

### Multiple Instances with Different Configs

Deploy multiple instances with different configurations:

```bash
# Instance 1: Development
helm install s3-dev . -f dev-values.yaml -n dev

# Instance 2: Production
helm install s3-prod . -f values-production.yaml -n prod
```

### External ConfigMap

If you want to manage the ConfigMap separately (not recommended):

1. Create your own ConfigMap
2. Disable the chart's ConfigMap generation (requires chart modification)
3. Reference your ConfigMap in the deployment

## See Also

- [Main README](../../README.md) - Project overview
- [Configuration Guide](../../CONFIGURATION.md) - Application configuration reference
- [values.yaml](values.yaml) - Default values
- [values-production.yaml](values-production.yaml) - Production example

