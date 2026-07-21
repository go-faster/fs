# fs [![Go Reference](https://img.shields.io/badge/go-pkg-00ADD8)](https://pkg.go.dev/github.com/go-faster/fs#section-documentation) [![codecov](https://img.shields.io/codecov/c/github/go-faster/fs?label=cover)](https://codecov.io/gh/go-faster/fs) [![experimental](https://img.shields.io/badge/-experimental-blueviolet)](https://go-faster.org/docs/projects/status#experimental)

Simple S3-compatible storage server for development and testing.

## Features

### S3-Compatible Storage Server

A lightweight S3-compatible storage server for development and testing.

**Quick Start:**
```bash
# Install
go install github.com/go-faster/fs/cmd/fs@latest

# Start the server
fs s3

# Or with custom configuration
fs s3 --addr :9000 --root /data/s3
```

**Features:**
- Bucket operations (create, delete, list)
- Object operations (put, get, delete, list, copy, tagging, metadata)
- Multipart uploads
- File system-based storage
- Compatible with AWS CLI, MinIO client, and other S3 clients
- Health check endpoint

Compatibility is measured against the upstream
[ceph/s3-tests](https://github.com/ceph/s3-tests) suite and real S3 clients —
see the [S3 conformance report](docs/CONFORMANCE.md).

**Example Usage:**
```bash
# Using AWS CLI
export AWS_ENDPOINT_URL=http://localhost:8080
aws s3 mb s3://mybucket --endpoint-url $AWS_ENDPOINT_URL
aws s3 cp file.txt s3://mybucket/ --endpoint-url $AWS_ENDPOINT_URL

# Using cURL
curl -X PUT http://localhost:8080/mybucket
curl -X PUT -d "Hello!" http://localhost:8080/mybucket/hello.txt
curl http://localhost:8080/mybucket/hello.txt
```

## Authentication & TLS

The binary authenticates requests with **AWS Signature V4 by default**. Provide
a root credential and (optionally) TLS:

```bash
export FS_ROOT_ACCESS_KEY=AKIAEXAMPLE
export FS_ROOT_SECRET_KEY=exampleSecretKey
fs s3 --tls-cert cert.pem --tls-key key.pem
```

Additional keys, per-bucket grants (`read`/`write`/`admin`), and public-read
buckets are configured under `auth:` in the config file. To run without any
authentication (development only), pass `--insecure-no-auth`.

SigV4 header auth, presigned URLs (≤7-day expiry) and streaming uploads are all
verified; TLS certificates hot-reload without dropping connections. As a
library, enable it with `server.WithAuth(store)` / `server.WithCORS(cfg)` — the
bare handler stays anonymous unless you opt in.

## Installation

```bash
go install github.com/go-faster/fs/cmd/fs@latest
```

Or build from source:
```bash
git clone https://github.com/go-faster/fs
cd fs
go build -o bin/fs ./cmd/fs
```

## Usage

### Quick Start

```bash
# Start S3 server with defaults
fs s3

# Show help
fs s3 --help
```

### Configuration

The server supports both YAML configuration files and command-line flags:

```bash
# Using YAML configuration
fs s3 --config config.yaml

# Using command-line flags
fs s3 --addr :9000 --root /var/lib/s3data

# Mix both (flags override config file)
fs s3 --config config.yaml --addr :9000

# Generate example configuration
fs s3 --generate-config > my-config.yaml
```

Run `fs s3 --generate-config` to produce a fully commented configuration template, and `fs s3 --help` for the list of flags.

### Example Configuration

```yaml
server:
  addr: ":8080"
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s
  health_path: "/health"

storage:
  root: ".s3data"
  type: "filesystem"

observability:
  service_name: "go-faster/fs"
  enable_request_logging: true
  enable_metrics: true
  enable_tracing: true
```

## Use as a library

The S3 server is embeddable. Install the module and pick a storage backend —
[`storagefs`](storagefs) for the filesystem, [`storagemem`](storagemem) for
in-memory, or your own implementation of the [`fs.Storage`](storage.go)
interface:

```bash
go get github.com/go-faster/fs
```

The library core pulls in **no observability stack** — wrap the handler yourself
(e.g. with `otelhttp`) via `server.NewHandler` or `Config.WrapHandler`.

Custom backends can verify themselves against the storage contract with the
[`storagetest`](storagetest) conformance suite:

```go
func TestStorage(t *testing.T) {
	storagetest.Run(t, func(t testing.TB) fs.Storage {
		return mybackend.New(t.TempDir())
	})
}
```

### Mount the handler into your own server

Use `server.NewHandler` when you already run an `http.Server` or mux and just
want to expose the S3 API (optionally under a path prefix):

```go
package main

import (
	"net/http"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagefs"
)

func main() {
	store, err := storagefs.New("/data")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/s3/", http.StripPrefix("/s3", server.NewHandler(store)))

	http.ListenAndServe(":8080", mux)
}
```

### Run the turnkey server

Use `server.New` for a managed server with a health endpoint, request timeouts,
optional bucket pre-creation and graceful shutdown driven by a `context`:

```go
package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(server.Config{
		Storage: storagemem.New(),
		Addr:    ":9000",
		Buckets: []string{"uploads"}, // pre-created if absent
	})
	if err != nil {
		panic(err)
	}

	// Serves until ctx is canceled, then drains in-flight requests.
	if err := srv.ListenAndServe(ctx); err != nil {
		panic(err)
	}
}
```

### `server.Config`

| Field | Default | Description |
|-------|---------|-------------|
| `Storage` | — (required) | Backend serving S3 operations (`fs.Storage`). |
| `Addr` | `:8080` | TCP address to listen on. |
| `ReadTimeout` / `WriteTimeout` / `IdleTimeout` | `30s` / `30s` / `120s` | Underlying `http.Server` timeouts. |
| `HealthPath` | `/health` | Plaintext health endpoint; `"-"` disables it. |
| `Buckets` | — | Buckets created (idempotently) before serving. |
| `WrapHandler` | — | Wrap the handler with middleware/observability (e.g. `otelhttp.NewHandler`). |

See the [`server` package reference](https://pkg.go.dev/github.com/go-faster/fs/server)
for the full API and runnable examples.

## Development

```bash
# Run tests
go test ./...

# Build
go build ./cmd/fs

# Run with coverage
make coverage
```

## License

Apache 2.0
