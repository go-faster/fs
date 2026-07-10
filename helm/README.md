# Helm chart packaging

This directory contains the [go-faster-fs chart](go-faster-fs/) and a Makefile
for packaging and distributing it as an OCI artifact.

## Usage

The Makefile is self-documenting:

```bash
make help
```

Common workflows:

```bash
# Lint and render templates
make test

# Package the chart (produces go-faster-fs-<version>.tgz)
make package

# Push to an OCI registry (defaults to ghcr.io/go-faster)
GITHUB_USER=<user> GITHUB_TOKEN=<token> make login-ghcr
make push

# Full release: clean, test, package, push
make release
```

Override the target registry with `REGISTRY=<registry> make push`.

Install the published chart:

```bash
helm install my-fs oci://ghcr.io/go-faster/go-faster-fs --version <version>
```

See the [chart README](go-faster-fs/README.md) for deployment configuration.
