# OCI Helm Chart Build Guide

This guide explains how to build, package, and distribute the go-faster-fs Helm chart as an OCI (Open Container Initiative) artifact.

## What is OCI for Helm?

OCI (Open Container Initiative) support in Helm allows you to store Helm charts in OCI-compliant container registries like:
- GitHub Container Registry (ghcr.io)
- Docker Hub
- Google Artifact Registry
- Azure Container Registry
- AWS Elastic Container Registry
- Harbor
- GitLab Container Registry

## Prerequisites

- Helm 3.8.0 or later (OCI support is stable)
- Access to an OCI-compatible registry
- Registry credentials configured

## Quick Start

### 1. Package the Chart

```bash
cd /src/faster/fs/helm
helm package go-faster-fs
```

This creates: `go-faster-fs-0.1.0.tgz`

### 2. Login to Registry

```bash
# GitHub Container Registry
echo $GITHUB_TOKEN | helm registry login ghcr.io -u USERNAME --password-stdin

# Docker Hub
echo $DOCKER_PASSWORD | helm registry login registry-1.docker.io -u USERNAME --password-stdin

# Or use interactive login
helm registry login ghcr.io
```

### 3. Push to OCI Registry

```bash
# Push to GitHub Container Registry
helm push go-faster-fs-0.1.0.tgz oci://ghcr.io/go-faster

# Push to Docker Hub
helm push go-faster-fs-0.1.0.tgz oci://registry-1.docker.io/your-username
```

### 4. Install from OCI Registry

```bash
# Install directly from OCI registry
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Or with custom values
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0 -f values.yaml
```

## Detailed Instructions

### Building the Chart

#### 1. Validate the Chart

Before packaging, ensure the chart is valid:

```bash
cd /src/faster/fs/helm/go-faster-fs
helm lint .
```

#### 2. Package the Chart

```bash
cd /src/faster/fs/helm
helm package go-faster-fs
```

Output:
```
Successfully packaged chart and saved it to: /src/faster/fs/helm/go-faster-fs-0.1.0.tgz
```

#### 3. Verify the Package

```bash
# List contents
tar -tzf go-faster-fs-0.1.0.tgz | head -20

# Extract and inspect (optional)
tar -xzf go-faster-fs-0.1.0.tgz
```

### Pushing to Different Registries

#### GitHub Container Registry (ghcr.io)

```bash
# 1. Create Personal Access Token (PAT) with write:packages scope
#    https://github.com/settings/tokens

# 2. Login
export GITHUB_TOKEN="your_token_here"
echo $GITHUB_TOKEN | helm registry login ghcr.io -u your-username --password-stdin

# 3. Push chart
helm push go-faster-fs-0.1.0.tgz oci://ghcr.io/go-faster

# 4. Make public (optional, via GitHub UI)
#    Go to: https://github.com/orgs/go-faster/packages
```

#### Docker Hub

```bash
# 1. Login
docker login
# OR
echo $DOCKER_PASSWORD | helm registry login registry-1.docker.io -u your-username --password-stdin

# 2. Push chart
helm push go-faster-fs-0.1.0.tgz oci://registry-1.docker.io/your-username

# 3. Chart will be available at:
#    https://hub.docker.com/r/your-username/go-faster-fs
```

#### Google Artifact Registry

```bash
# 1. Create repository
gcloud artifacts repositories create helm-charts \
    --repository-format=docker \
    --location=us-central1

# 2. Configure Docker auth
gcloud auth configure-docker us-central1-docker.pkg.dev

# 3. Login Helm
gcloud auth print-access-token | helm registry login \
    us-central1-docker.pkg.dev -u oauth2accesstoken --password-stdin

# 4. Push chart
helm push go-faster-fs-0.1.0.tgz \
    oci://us-central1-docker.pkg.dev/PROJECT_ID/helm-charts
```

#### Azure Container Registry

```bash
# 1. Login
az acr login --name myregistry

# 2. Login Helm
az acr login --name myregistry --expose-token | \
    jq -r .accessToken | \
    helm registry login myregistry.azurecr.io --username 00000000-0000-0000-0000-000000000000 --password-stdin

# 3. Push chart
helm push go-faster-fs-0.1.0.tgz oci://myregistry.azurecr.io/helm
```

#### AWS Elastic Container Registry

```bash
# 1. Create ECR repository
aws ecr create-repository --repository-name helm/go-faster-fs

# 2. Login
aws ecr get-login-password --region us-east-1 | \
    helm registry login --username AWS \
    --password-stdin 123456789012.dkr.ecr.us-east-1.amazonaws.com

# 3. Push chart
helm push go-faster-fs-0.1.0.tgz \
    oci://123456789012.dkr.ecr.us-east-1.amazonaws.com/helm
```

### Installing from OCI Registry

#### Basic Installation

```bash
# Install latest version
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs

# Install specific version
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Install with custom values
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs \
    --version 0.1.0 \
    -f custom-values.yaml
```

#### With Namespace

```bash
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs \
    --version 0.1.0 \
    --namespace s3-storage \
    --create-namespace
```

#### Production Installation

```bash
helm install production-fs oci://ghcr.io/go-faster/go-faster-fs \
    --version 0.1.0 \
    --namespace production \
    --create-namespace \
    -f values-production.yaml
```

### Upgrading from OCI Registry

```bash
# Upgrade to specific version
helm upgrade my-fs oci://ghcr.io/go-faster/go-faster-fs --version 0.2.0

# Upgrade with new values
helm upgrade my-fs oci://ghcr.io/go-faster/go-faster-fs \
    --version 0.2.0 \
    -f updated-values.yaml
```

### Managing OCI Charts

#### List Available Versions

```bash
# Using Helm (requires authentication)
helm show chart oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Using registry API (GitHub)
curl -H "Authorization: Bearer $GITHUB_TOKEN" \
    https://ghcr.io/v2/go-faster/go-faster-fs/tags/list
```

#### Pull Chart Locally

```bash
# Pull without installing
helm pull oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Pull and extract
helm pull oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0 --untar
```

#### Show Chart Information

```bash
# Show Chart.yaml
helm show chart oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Show values.yaml
helm show values oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0

# Show all information
helm show all oci://ghcr.io/go-faster/go-faster-fs --version 0.1.0
```

## Automation & CI/CD

### GitHub Actions Example

```yaml
name: Release Helm Chart

on:
  push:
    tags:
      - 'v*'

jobs:
  release:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write

    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Install Helm
        uses: azure/setup-helm@v3
        with:
          version: '3.14.0'

      - name: Package Chart
        run: |
          cd helm
          helm package go-faster-fs

      - name: Login to GHCR
        run: |
          echo ${{ secrets.GITHUB_TOKEN }} | helm registry login ghcr.io -u ${{ github.actor }} --password-stdin

      - name: Push Chart
        run: |
          cd helm
          helm push go-faster-fs-*.tgz oci://ghcr.io/${{ github.repository_owner }}
```

### GitLab CI Example

```yaml
release_chart:
  stage: release
  image: alpine/helm:latest
  script:
    - cd helm
    - helm package go-faster-fs
    - echo $CI_REGISTRY_PASSWORD | helm registry login $CI_REGISTRY -u $CI_REGISTRY_USER --password-stdin
    - helm push go-faster-fs-*.tgz oci://$CI_REGISTRY/$CI_PROJECT_PATH
  only:
    - tags
```

### Makefile Example

```makefile
CHART_NAME := go-faster-fs
CHART_VERSION := $(shell grep '^version:' helm/$(CHART_NAME)/Chart.yaml | awk '{print $$2}')
REGISTRY := ghcr.io/go-faster

.PHONY: helm-package
helm-package:
	cd helm && helm package $(CHART_NAME)

.PHONY: helm-lint
helm-lint:
	helm lint helm/$(CHART_NAME)

.PHONY: helm-push
helm-push: helm-package
	helm push helm/$(CHART_NAME)-$(CHART_VERSION).tgz oci://$(REGISTRY)

.PHONY: helm-install-local
helm-install-local:
	helm install test-fs helm/$(CHART_NAME) -f helm/$(CHART_NAME)/values.yaml

.PHONY: helm-install-oci
helm-install-oci:
	helm install test-fs oci://$(REGISTRY)/$(CHART_NAME) --version $(CHART_VERSION)
```

## Best Practices

### 1. Versioning

- Follow Semantic Versioning (SemVer)
- Increment `version` in `Chart.yaml` for each release
- Update `appVersion` when application version changes
- Use git tags matching chart versions

### 2. Chart Metadata

Ensure `Chart.yaml` is complete:

```yaml
apiVersion: v2
name: go-faster-fs
description: S3-compatible storage server with filesystem backend
type: application
version: 0.1.0
appVersion: "0.1.0"
home: https://github.com/go-faster/fs
sources:
  - https://github.com/go-faster/fs
maintainers:
  - name: go-faster
    email: contact@go-faster.org
keywords:
  - s3
  - storage
  - filesystem
  - object-storage
```

### 3. Documentation

Include in chart:
- `README.md` - Installation and usage
- `CONFIGURATION.md` - Configuration reference
- `values.yaml` - Well-commented defaults

### 4. Security

- Sign charts with `helm package --sign`
- Use provenance files
- Scan charts for vulnerabilities
- Use minimal container images
- Set appropriate security contexts

### 5. Testing

```bash
# Template validation
helm template test ./helm/go-faster-fs --debug

# Lint
helm lint ./helm/go-faster-fs

# Dry-run install
helm install test ./helm/go-faster-fs --dry-run --debug

# Test with values
helm template test ./helm/go-faster-fs -f test-values.yaml
```

## Troubleshooting

### Authentication Issues

```bash
# Clear Helm registry credentials
rm ~/.config/helm/registry/config.json

# Re-login
helm registry login ghcr.io
```

### Push Failures

```bash
# Check package integrity
tar -tzf go-faster-fs-0.1.0.tgz

# Verify registry URL format
# Correct: oci://ghcr.io/go-faster
# Wrong: oci://ghcr.io/go-faster/go-faster-fs (don't include chart name)
```

### Version Conflicts

```bash
# Check existing versions
helm search repo go-faster-fs --versions

# Force push (overwrite, if registry allows)
# Note: Not all registries support overwriting tags
```

## Registry-Specific Notes

### GitHub Container Registry

- Charts are private by default
- Make public: Repository Settings → Packages → Change visibility
- URL format: `oci://ghcr.io/OWNER/CHART_NAME`
- Supports anonymous pulls for public charts

### Docker Hub

- Charts appear as Docker images
- URL format: `oci://registry-1.docker.io/USERNAME/CHART_NAME`
- Free tier has pull rate limits

### Harbor

- Supports Helm-specific UI
- Can scan charts for vulnerabilities
- Supports replication
- URL format: `oci://harbor.example.com/PROJECT_NAME`

## Migration from Traditional Helm Repos

### From HTTP-based Helm Repo

```bash
# Old way
helm repo add go-faster https://go-faster.github.io/helm-charts
helm install my-fs go-faster/go-faster-fs

# New way (OCI)
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs
```

### Advantages of OCI

- No need for separate chart repository server
- Unified container and chart management
- Better integration with existing registry infrastructure
- Improved security and authentication
- Support for chart provenance and signing

## See Also

- [Helm OCI Documentation](https://helm.sh/docs/topics/registries/)
- [Chart Testing](https://github.com/helm/chart-testing)
- [Chart Releaser](https://github.com/helm/chart-releaser)
- [Main README](../../README.md)
- [Helm Chart README](README.md)

