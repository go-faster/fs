# Quick Start Guide: S3-Compatible Storage Server

This guide will help you get started with the S3-compatible storage server in under 5 minutes.

## Step 1: Build the Server

```bash
cd /src/faster/fs
go build -o bin/fs ./cmd/fs
```

## Step 2: Start the Server

```bash
./bin/fs s3
```

You should see:
```
S3-compatible server starting on :8080
Storage root: /src/faster/fs/.s3data
Health check available at http://:8080/health

Press Ctrl+C to stop the server
```

## Step 3: Test with cURL

Open a new terminal and try these commands:

### Create a bucket
```bash
curl -X PUT http://localhost:8080/mybucket
```

### Upload a file
```bash
echo "Hello, S3!" | curl -X PUT -d @- http://localhost:8080/mybucket/hello.txt
```

### Download the file
```bash
curl http://localhost:8080/mybucket/hello.txt
```

Expected output: `Hello, S3!`

### List objects in the bucket
```bash
curl http://localhost:8080/mybucket
```

You should see XML output listing your `hello.txt` object.

### Delete the file
```bash
curl -X DELETE http://localhost:8080/mybucket/hello.txt
```

### Delete the bucket
```bash
curl -X DELETE http://localhost:8080/mybucket
```

## Step 4: Test with AWS CLI (Optional)

If you have AWS CLI installed:

```bash
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test

# Create bucket
aws s3 mb s3://test-bucket --endpoint-url http://localhost:8080

# Upload file
echo "Testing AWS CLI" > test.txt
aws s3 cp test.txt s3://test-bucket/ --endpoint-url http://localhost:8080

# List objects
aws s3 ls s3://test-bucket/ --endpoint-url http://localhost:8080

# Download file
aws s3 cp s3://test-bucket/test.txt downloaded.txt --endpoint-url http://localhost:8080

# Clean up
aws s3 rm s3://test-bucket/test.txt --endpoint-url http://localhost:8080
aws s3 rb s3://test-bucket --endpoint-url http://localhost:8080
```

## Server Options

### Custom Port
```bash
./bin/fs s3 --addr :9000
```

### Custom Storage Directory
```bash
./bin/fs s3 --root /data/s3storage
```

### Combine Options
```bash
./bin/fs s3 --addr :9000 --root /data/s3storage
```

## Health Check

Check if the server is running:
```bash
curl http://localhost:8080/health
```

Expected output: `OK`

## Data Location

By default, data is stored in `.s3data` directory:
```
.s3data/
  └── mybucket/
      ├── hello.txt
      └── subdir/
          └── file.txt
```

## Stopping the Server

Press `Ctrl+C` in the terminal where the server is running.

## Next Steps

- Check out the [examples](examples/) directory for more usage examples
- Read the [S3 README](S3_README.md) for detailed documentation
- Try different S3 clients (MinIO mc, s3cmd, etc.)

## Common Issues

### Address already in use
If port 8080 is busy, use a different port:
```bash
./bin/fs s3 --addr :9090
```

### Permission denied
Make sure you have write permissions in the current directory, or specify a different root:
```bash
./bin/fs s3 --root ~/s3data
```

## Summary

You now have a working S3-compatible storage server! The basic workflow is:

1. **Start server**: `./bin/fs s3`
2. **Create bucket**: `PUT /bucket-name`
3. **Upload object**: `PUT /bucket-name/object-key`
4. **Download object**: `GET /bucket-name/object-key`
5. **List objects**: `GET /bucket-name`
6. **Delete object**: `DELETE /bucket-name/object-key`
7. **Delete bucket**: `DELETE /bucket-name`

Happy storing! 🚀

