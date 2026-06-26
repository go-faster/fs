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
- Object operations (put, get, delete, list)
- File system-based storage
- Compatible with AWS CLI, MinIO client, and other S3 clients
- Health check endpoint

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

See [S3_README.md](S3_README.md) for detailed documentation.

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

See [CONFIGURATION.md](CONFIGURATION.md) for detailed configuration documentation.

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

The S3 server is embeddable. Pick a storage backend (`storagefs` for filesystem,
`storagemem` for in-memory, or your own implementation of [`fs.Storage`](storage.go))
and either mount the bare `http.Handler` or run the turnkey `server.Server`. The
library core pulls in no observability stack — wrap the handler yourself (e.g. with
`otelhttp`) via `Config.WrapHandler`.

```go
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

	// Low-level: mount into your own mux.
	h := server.NewHandler(store)
	http.ListenAndServe(":8080", h)

	// High-level: turnkey server with health, timeouts and graceful shutdown.
	srv, err := server.New(server.Config{Storage: store, Addr: ":9000"})
	if err != nil {
		panic(err)
	}
	srv.ListenAndServe(ctx) // serves until ctx is canceled, then drains
}
```

See the [`server` package reference](https://pkg.go.dev/github.com/go-faster/fs/server)
for the full API and runnable examples.

## Development

```bash
# Run tests
go test ./...

# Build
go build ./cmd/fs

# Run with coverage
./go.coverage.sh
```

## License

Apache 2.0
