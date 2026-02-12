# go-faster-fs Helm Chart

This Helm chart deploys an S3-compatible storage server with filesystem backend to Kubernetes.

## Features

- S3-compatible API for object storage
- Filesystem-based storage backend
- Kubernetes-native deployment
- Configurable storage (emptyDir or PersistentVolumeClaim)
- Health checks and probes
- Optional ingress and Gateway API support
- Horizontal Pod Autoscaling support

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+

## Installing the Chart

To install the chart with the release name `my-fs`:

```bash
helm install my-fs ./go-faster-fs
```

## Uninstalling the Chart

To uninstall/delete the `my-fs` deployment:

```bash
helm delete my-fs
```

## Configuration

The chart uses a ConfigMap to manage application configuration. All configuration is defined in YAML format in the `values.yaml` file under the `config` section.

### Configuration Parameters

#### Server Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `config.server.addr` | Server listen address | `":8080"` |
| `config.server.readTimeout` | HTTP read timeout | `"30s"` |
| `config.server.writeTimeout` | HTTP write timeout | `"30s"` |
| `config.server.idleTimeout` | HTTP idle timeout | `"2m0s"` |
| `config.server.healthPath` | Health check endpoint path | `"/health"` |

#### Storage Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `config.storage.root` | Root directory for S3 storage | `"/data"` |
| `config.storage.type` | Storage backend type | `"filesystem"` |
| `config.storage.buckets` | List of buckets to pre-create on startup | `[]` |

#### Observability Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `config.observability.serviceName` | Service name for telemetry | `"go-faster/fs"` |
| `config.observability.enableRequestLogging` | Enable HTTP request logging | `true` |
| `config.observability.enableMetrics` | Enable Prometheus metrics | `true` |
| `config.observability.enableTracing` | Enable OpenTelemetry tracing | `true` |

#### General Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas | `1` |
| `image.repository` | Image repository | `ghcr.io/go-faster/fs` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `image.tag` | Image tag (defaults to chart appVersion) | `""` |
| `service.type` | Kubernetes service type | `ClusterIP` |
| `service.port` | Service port | `8080` |
| `persistence.enabled` | Enable persistent storage | `true` |
| `persistence.emptyDir` | Use emptyDir (ephemeral) storage | `true` |
| `persistence.storageClass` | Storage class for PVC | `""` |
| `persistence.accessMode` | PVC access mode | `ReadWriteOnce` |
| `persistence.size` | PVC size | `10Gi` |
| `persistence.existingClaim` | Use existing PVC | `""` |
| `resources.limits.cpu` | CPU limit | `1000m` |
| `resources.limits.memory` | Memory limit | `512Mi` |
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `128Mi` |
| `ingress.enabled` | Enable ingress | `false` |
| `autoscaling.enabled` | Enable horizontal pod autoscaling | `false` |

Specify each parameter using the `--set key=value[,key=value]` argument to `helm install`. For example:

```bash
helm install my-fs ./go-faster-fs \
  --set persistence.emptyDir=false \
  --set persistence.size=20Gi
```

Alternatively, a YAML file that specifies the values for the parameters can be provided while installing the chart:

```bash
helm install my-fs ./go-faster-fs -f values.yaml
```

### ConfigMap-Based Configuration

The chart automatically creates a ConfigMap containing the application configuration. The entire `config` section from `values.yaml` is rendered as a YAML configuration file and mounted into the pod at `/etc/fs/config.yaml`.

Example configuration in `values.yaml`:

```yaml
config:
  server:
    addr: ":9000"
    readTimeout: "60s"
    writeTimeout: "2m0s"
    idleTimeout: "5m0s"
  storage:
    root: "/data"
    type: "filesystem"
  observability:
    serviceName: "my-s3-server"
    enableRequestLogging: true
    enableMetrics: true
    enableTracing: true
```

This will be automatically converted to a ConfigMap and mounted into the application. Configuration changes require a pod restart to take effect.

#### Pre-Creating Buckets

You can configure buckets to be automatically created when the server starts:

```yaml
config:
  storage:
    root: "/data"
    buckets:
      - uploads
      - backups
      - temp-files
```

This is useful for:
- Ensuring required buckets exist before applications connect
- Kubernetes deployments with init requirements
- Simplifying setup in development/staging environments

To view the generated ConfigMap:

```bash
kubectl get configmap <release-name>-go-faster-fs-config -o yaml
```

For detailed configuration options, see [CONFIGURATION.md](CONFIGURATION.md).

## Storage Configuration

### Ephemeral Storage (Default)

By default, the chart uses `emptyDir` for storage, which means data is lost when the pod is deleted:

```yaml
persistence:
  enabled: true
  emptyDir: true
```

### Persistent Storage

To use persistent storage with a PersistentVolumeClaim:

```yaml
persistence:
  enabled: true
  emptyDir: false
  storageClass: "standard"
  size: 20Gi
```

### Existing PVC

To use an existing PersistentVolumeClaim:

```yaml
persistence:
  enabled: true
  emptyDir: false
  existingClaim: "my-existing-pvc"
```

## Using the S3 Server

Once deployed, you can use any S3-compatible client to interact with the server:

### With AWS CLI

```bash
# Port forward to access the service
kubectl port-forward svc/my-fs-go-faster-fs 8080:8080

# Configure AWS CLI
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_ENDPOINT_URL=http://localhost:8080

# Create a bucket
aws s3 mb s3://my-bucket --endpoint-url=$AWS_ENDPOINT_URL

# Upload a file
aws s3 cp myfile.txt s3://my-bucket/ --endpoint-url=$AWS_ENDPOINT_URL

# List objects
aws s3 ls s3://my-bucket/ --endpoint-url=$AWS_ENDPOINT_URL

# Download a file
aws s3 cp s3://my-bucket/myfile.txt downloaded.txt --endpoint-url=$AWS_ENDPOINT_URL
```

### With minio-go (Go)

```go
package main

import (
    "context"
    "log"

    "github.com/minio/minio-go/v7"
    "github.com/minio/minio-go/v7/pkg/credentials"
)

func main() {
    client, err := minio.New("localhost:8080", &minio.Options{
        Creds:  credentials.NewStaticV4("test", "test", ""),
        Secure: false,
    })
    if err != nil {
        log.Fatal(err)
    }

    // Use the client...
    ctx := context.Background()
    err = client.MakeBucket(ctx, "my-bucket", minio.MakeBucketOptions{})
    if err != nil {
        log.Fatal(err)
    }
}
```

## Ingress

To enable ingress:

```yaml
ingress:
  enabled: true
  className: "nginx"
  hosts:
    - host: fs.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: fs-tls
      hosts:
        - fs.example.com
```

## Autoscaling

To enable horizontal pod autoscaling:

```yaml
autoscaling:
  enabled: true
  minReplicas: 1
  maxReplicas: 10
  targetCPUUtilizationPercentage: 80
```

## Security Context

The chart runs the container as a non-root user (UID 1000) by default:

```yaml
podSecurityContext:
  fsGroup: 1000
  runAsUser: 1000
  runAsGroup: 1000
  runAsNonRoot: true

securityContext:
  capabilities:
    drop:
    - ALL
  readOnlyRootFilesystem: false
  runAsNonRoot: true
  runAsUser: 1000
  allowPrivilegeEscalation: false
```

## License

This chart is licensed under the same terms as the go-faster/fs project.

## Links

- [go-faster/fs GitHub Repository](https://github.com/go-faster/fs)
- [Helm Documentation](https://helm.sh/docs/)
- [S3 API Documentation](https://docs.aws.amazon.com/s3/index.html)

