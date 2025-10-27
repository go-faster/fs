# S3 Server Examples

This directory contains example scripts demonstrating how to use the S3-compatible storage server with various S3 clients.

## Prerequisites

First, start the S3 server:

```bash
# Build the fs binary
cd /src/faster/fs
go build -o bin/fs ./cmd/fs

# Start the server
./bin/fs s3
```

The server will start on `http://localhost:8080` by default.

## Examples

### 1. AWS CLI Example

Demonstrates using the AWS CLI with the S3 server.

**Requirements:**
- AWS CLI installed (`pip install awscli` or use your package manager)

**Run:**
```bash
./aws-cli-example.sh
```

**What it does:**
- Creates a bucket
- Uploads files
- Lists objects
- Downloads files
- Syncs directories
- Cleans up

### 2. cURL Example

Demonstrates using raw HTTP requests with cURL.

**Requirements:**
- cURL (usually pre-installed)

**Run:**
```bash
./curl-example.sh
```

**What it does:**
- Creates buckets using PUT requests
- Uploads objects with PUT + data
- Lists buckets and objects
- Downloads objects with GET
- Deletes objects and buckets

### 3. MinIO Client Example

Demonstrates using the MinIO client (mc) with the S3 server.

**Requirements:**
- MinIO client installed (https://min.io/docs/minio/linux/reference/minio-mc.html)

**Install MinIO client:**
```bash
# Linux
wget https://dl.min.io/client/mc/release/linux-amd64/mc
chmod +x mc
sudo mv mc /usr/local/bin/

# macOS
brew install minio/stable/mc
```

**Run:**
```bash
./minio-client-example.sh
```

**What it does:**
- Configures MinIO client alias
- Creates buckets
- Uploads and downloads files
- Mirrors directories
- Lists and removes objects

## Manual Testing

You can also test manually:

### Using cURL

```bash
# Create bucket
curl -X PUT http://localhost:8080/mybucket

# Upload file
echo "Hello, World!" | curl -X PUT -d @- http://localhost:8080/mybucket/hello.txt

# Download file
curl http://localhost:8080/mybucket/hello.txt

# List objects
curl http://localhost:8080/mybucket

# Delete file
curl -X DELETE http://localhost:8080/mybucket/hello.txt
```

### Using AWS CLI

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_ENDPOINT_URL=http://localhost:8080

aws s3 mb s3://mybucket --endpoint-url $AWS_ENDPOINT_URL
aws s3 cp file.txt s3://mybucket/ --endpoint-url $AWS_ENDPOINT_URL
aws s3 ls s3://mybucket/ --endpoint-url $AWS_ENDPOINT_URL
```

## Health Check

Check if the server is running:

```bash
curl http://localhost:8080/health
```

## Troubleshooting

### Server not responding
- Make sure the server is running: `./bin/fs s3`
- Check the server address: default is `:8080`

### Permission denied
- Make sure example scripts are executable: `chmod +x *.sh`

### AWS CLI errors
- Set credentials even though they're not validated: `AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test`
- Always specify `--endpoint-url` for AWS CLI commands

### MinIO client errors
- Make sure the alias is configured: `mc alias set local http://localhost:8080 test test`
- Use `--insecure` flag if needed: `mc --insecure ls local`

## Notes

- This is a development/testing server - authentication is not enforced
- Any access key/secret can be used with clients that require them
- Data is stored in `.s3data` directory by default
- Server logs all requests with timestamps

