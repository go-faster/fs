# S3-Compatible Storage Server

This directory contains an implementation of a lightweight S3-compatible storage server.

## Features

The S3 server implements the following S3 operations:

### Bucket Operations
- **List Buckets**: `GET /`
- **Create Bucket**: `PUT /{bucket}`
- **Delete Bucket**: `DELETE /{bucket}`

### Object Operations
- **List Objects**: `GET /{bucket}?prefix={prefix}`
- **Put Object**: `PUT /{bucket}/{key}`
- **Get Object**: `GET /{bucket}/{key}`
- **Delete Object**: `DELETE /{bucket}/{key}`

## Usage

### Start the Server

```bash
# Start with default settings (port 8080, data in .s3data)
fs s3

# Start on custom port with custom data directory
fs s3 --addr :9000 --root /var/lib/s3data

# Bind to specific interface
fs s3 --addr 127.0.0.1:8080
```

### Using with AWS CLI

You can use the AWS CLI to interact with the server:

```bash
# Configure AWS CLI (use any values for access key/secret)
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_ENDPOINT_URL=http://localhost:8080

# Create a bucket
aws s3 mb s3://mybucket --endpoint-url $AWS_ENDPOINT_URL

# Upload a file
aws s3 cp file.txt s3://mybucket/ --endpoint-url $AWS_ENDPOINT_URL

# List objects
aws s3 ls s3://mybucket/ --endpoint-url $AWS_ENDPOINT_URL

# Download a file
aws s3 cp s3://mybucket/file.txt downloaded.txt --endpoint-url $AWS_ENDPOINT_URL

# Delete a file
aws s3 rm s3://mybucket/file.txt --endpoint-url $AWS_ENDPOINT_URL
```

### Using with cURL

```bash
# List buckets
curl http://localhost:8080/

# Create a bucket
curl -X PUT http://localhost:8080/mybucket

# Upload an object
curl -X PUT -d "Hello, World!" http://localhost:8080/mybucket/hello.txt

# Download an object
curl http://localhost:8080/mybucket/hello.txt

# List objects in a bucket
curl http://localhost:8080/mybucket

# Delete an object
curl -X DELETE http://localhost:8080/mybucket/hello.txt

# Delete a bucket
curl -X DELETE http://localhost:8080/mybucket
```

### Using with MinIO Client (mc)

```bash
# Configure MinIO client
mc alias set local http://localhost:8080 test test

# Create a bucket
mc mb local/mybucket

# Upload a file
mc cp file.txt local/mybucket/

# List objects
mc ls local/mybucket

# Download a file
mc cp local/mybucket/file.txt downloaded.txt

# Remove a file
mc rm local/mybucket/file.txt
```

## Architecture

The server is built with:
- **Storage Backend**: File system-based storage
- **HTTP Server**: Standard library `net/http`
- **Concurrency**: Read-write mutex for thread-safe operations

### Directory Structure

```
{root}/
  ├── bucket1/
  │   ├── file1.txt
  │   └── dir/
  │       └── file2.txt
  └── bucket2/
      └── data.json
```

Each bucket is represented as a directory, and objects are stored as files maintaining the S3 key structure.

## Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `:8080` | Address to listen on |
| `--root` | `.s3data` | Root directory for S3 storage |
| `--log-level` | `info` | Log level (debug, info, warn, error) |

## Health Check

A health check endpoint is available at:
```bash
curl http://localhost:8080/health
```

## Limitations

This is a lightweight implementation intended for development and testing. It has the following limitations:

1. **No Authentication**: The server does not implement S3 authentication
2. **No Multipart Uploads**: Large file uploads should use the simple PUT operation
3. **No Versioning**: Object versioning is not supported
4. **No ACLs**: Access control lists are not implemented
5. **Basic XML Responses**: Only minimal S3 XML responses are implemented
6. **No Encryption**: Data is stored unencrypted on disk
7. **Single Node**: No distributed or replicated storage

## Development

### Testing

You can test the server with the included test suite:

```bash
go test ./...
```

### Building

```bash
go build -o bin/fs ./cmd/fs
```

## License

This project follows the same license as the parent repository.

