# fs [![Go Reference](https://img.shields.io/badge/go-pkg-00ADD8)](https://pkg.go.dev/github.com/go-faster/fs#section-documentation) [![codecov](https://img.shields.io/codecov/c/github/go-faster/fs?label=cover)](https://codecov.io/gh/go-faster/fs) [![experimental](https://img.shields.io/badge/-experimental-blueviolet)](https://go-faster.org/docs/projects/status#experimental)

File system utilities for Go, including an S3-compatible storage server.

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

```bash
# Start S3 server with defaults
fs s3

# Show help
fs s3 --help

# Custom configuration
fs s3 --addr :9000 --root /var/lib/s3data
```

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
