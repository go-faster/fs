# Quick Start Guide

This guide will help you get started with the go-faster-fs Helm chart.

## Prerequisites

- Kubernetes cluster (1.19+)
- Helm 3.0+
- kubectl configured to access your cluster

## Installation

### 1. Basic Installation (Development)

For development/testing with ephemeral storage:

```bash
helm install my-fs ./go-faster-fs
```

This will deploy the S3 server with:
- 1 replica
- emptyDir storage (data lost on pod restart)
- ClusterIP service on port 8080

### 2. Production Installation

For production with persistent storage:

```bash
helm install my-fs ./go-faster-fs \
  -f go-faster-fs/values-production.yaml
```

Or customize the values:

```bash
helm install my-fs ./go-faster-fs \
  --set persistence.emptyDir=false \
  --set persistence.size=100Gi \
  --set persistence.storageClass=fast-ssd \
  --set autoscaling.enabled=true \
  --set autoscaling.maxReplicas=5
```

## Accessing the S3 Server

### Port Forward (Development)

```bash
kubectl port-forward svc/my-fs-go-faster-fs 8080:8080
```

### Using AWS CLI

```bash
# Set environment variables
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_ENDPOINT_URL=http://localhost:8080

# Create a bucket
aws s3 mb s3://my-bucket --endpoint-url=$AWS_ENDPOINT_URL

# List buckets
aws s3 ls --endpoint-url=$AWS_ENDPOINT_URL

# Upload a file
echo "Hello World" > test.txt
aws s3 cp test.txt s3://my-bucket/ --endpoint-url=$AWS_ENDPOINT_URL

# List objects
aws s3 ls s3://my-bucket/ --endpoint-url=$AWS_ENDPOINT_URL

# Download the file
aws s3 cp s3://my-bucket/test.txt downloaded.txt --endpoint-url=$AWS_ENDPOINT_URL
```

## Configuration Examples

### Enable Ingress

```bash
helm install my-fs ./go-faster-fs \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set ingress.hosts[0].host=s3.example.com \
  --set ingress.hosts[0].paths[0].path=/ \
  --set ingress.hosts[0].paths[0].pathType=Prefix
```

### Enable Autoscaling

```bash
helm install my-fs ./go-faster-fs \
  --set autoscaling.enabled=true \
  --set autoscaling.minReplicas=2 \
  --set autoscaling.maxReplicas=10 \
  --set autoscaling.targetCPUUtilizationPercentage=70
```

### Use Existing PVC

```bash
helm install my-fs ./go-faster-fs \
  --set persistence.existingClaim=my-existing-pvc
```

## Upgrading

```bash
helm upgrade my-fs ./go-faster-fs \
  --set replicaCount=2
```

## Uninstalling

```bash
helm uninstall my-fs
```

**Note:** If you used persistent storage, the PVC may not be automatically deleted. To delete it:

```bash
kubectl delete pvc my-fs-go-faster-fs
```

## Troubleshooting

### Check pod status

```bash
kubectl get pods -l app.kubernetes.io/name=go-faster-fs
```

### View logs

```bash
kubectl logs -l app.kubernetes.io/name=go-faster-fs --tail=100 -f
```

### Describe pod

```bash
kubectl describe pod -l app.kubernetes.io/name=go-faster-fs
```

### Test connection

```bash
helm test my-fs
```

## Common Issues

### Pod is in CrashLoopBackOff

Check the logs:
```bash
kubectl logs -l app.kubernetes.io/name=go-faster-fs
```

### Cannot mount PVC

Check if the PVC is bound:
```bash
kubectl get pvc
```

Describe the PVC to see events:
```bash
kubectl describe pvc my-fs-go-faster-fs
```

### Permission denied errors

Ensure the storage supports the fsGroup and runAsUser settings in podSecurityContext.

## Next Steps

- Configure ingress for external access
- Set up monitoring and metrics
- Configure backup for persistent data
- Review security settings for production use

For more details, see the [README.md](README.md).

