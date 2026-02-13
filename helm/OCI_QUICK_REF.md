# Helm OCI Quick Reference

Quick commands for working with the go-faster-fs Helm chart as an OCI artifact.

## Build & Package

```bash
# Navigate to helm directory
cd /src/faster/fs/helm

# Lint the chart
make lint

# Package the chart
make package

# Test everything
make test

# Verify package
make verify
```

## Push to Registry

### GitHub Container Registry (ghcr.io)

```bash
# Set environment variables
export GITHUB_USER="your-username"
export GITHUB_TOKEN="ghp_your_token_here"

# Login
make login-ghcr

# Push chart
make push REGISTRY=ghcr.io/go-faster
```

### Docker Hub

```bash
# Login
make login-docker

# Push
make push REGISTRY=registry-1.docker.io/your-username
```

### Custom Registry

```bash
# Login manually
helm registry login your-registry.com

# Push
make push REGISTRY=your-registry.com/your-org
```

## Install from OCI

```bash
# Install from GitHub Container Registry
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Install with custom values
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0 -f values.yaml

# Install in specific namespace
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs \
  --version 0.1.0 \
  --namespace s3-storage \
  --create-namespace

# Install production config
helm install prod-fs oci://ghcr.io/go-faster/go-faster-fs \
  --version 0.1.0 \
  -f values-production.yaml
```

## Manage Charts

```bash
# Pull chart locally
helm pull oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Pull and extract
helm pull oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0 --untar

# Show chart info
helm show chart oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Show values
helm show values oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Show all
helm show all oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0
```

## Upgrade & Rollback

```bash
# Upgrade to new version
helm upgrade my-fs oci://ghcr.io/go-faster/go-faster-fs --version 0.2.0

# Upgrade with new values
helm upgrade my-fs oci://ghcr.io/go-faster/go-faster-fs --version 0.2.0 -f new-values.yaml

# Rollback
helm rollback my-fs 1
```

## Makefile Commands

```bash
make help              # Show all available commands
make version           # Show chart version
make lint              # Lint the chart
make template          # Test template rendering
make package           # Package the chart
make verify            # Verify the package
make test              # Run all tests
make push              # Push to OCI registry
make clean             # Remove packaged files
make release           # Full release workflow
```

## Common Workflows

### Local Development

```bash
# Test locally
make test

# Install locally
helm install test-fs ./go-faster-fs -f go-faster-fs/values.yaml
```

### Release New Version

```bash
# 1. Update version in Chart.yaml
vim go-faster-fs/Chart.yaml

# 2. Run full release
make release REGISTRY=ghcr.io/go-faster

# Or step by step:
make clean
make test
make package
make push REGISTRY=ghcr.io/go-faster
```

### Use in Production

```bash
# Install with production values
helm install production-fs oci://ghcr.io/go-faster/go-faster-fs \
  --version 0.1.0 \
  --namespace production \
  --create-namespace \
  -f values-production.yaml
```

## Registry URLs

- **GitHub**: `oci://ghcr.io/OWNER/CHART_NAME`
- **Docker Hub**: `oci://registry-1.docker.io/USERNAME/CHART_NAME`
- **Google**: `oci://REGION-docker.pkg.dev/PROJECT/REPO`
- **AWS**: `oci://ACCOUNT.dkr.ecr.REGION.amazonaws.com/REPO`
- **Azure**: `oci://REGISTRY.azurecr.io/REPO`

## Environment Variables

```bash
# For GitHub Container Registry
export GITHUB_USER="your-username"
export GITHUB_TOKEN="ghp_token"

# For custom registry
export REGISTRY="your-registry.com/your-org"
```

## Troubleshooting

```bash
# Clear registry credentials
rm ~/.config/helm/registry/config.json

# Re-login
helm registry login ghcr.io

# Verify package contents
tar -tzf go-faster-fs-0.1.0.tgz | less

# Test template rendering
helm template test ./go-faster-fs --debug

# Dry-run install
helm install test ./go-faster-fs --dry-run --debug
```

## See Also

- [OCI Build Guide](OCI_BUILD_GUIDE.md) - Complete documentation
- [Chart README](go-faster-fs/README.md) - Chart usage
- [Configuration Guide](go-faster-fs/CONFIGURATION.md) - Configuration reference

