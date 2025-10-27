#!/bin/bash
# Example: Using the S3 server with AWS CLI

set -e

# Configuration
ENDPOINT="http://localhost:8080"
BUCKET="example-bucket"
export AWS_ACCESS_KEY_ID="test"
export AWS_SECRET_ACCESS_KEY="test"

echo "=== S3 Server Example with AWS CLI ==="
echo ""

# Create bucket
echo "1. Creating bucket: $BUCKET"
aws s3 mb "s3://$BUCKET" --endpoint-url "$ENDPOINT"
echo ""

# Upload a file
echo "2. Creating and uploading a test file"
echo "Hello from AWS CLI!" > /tmp/test.txt
aws s3 cp /tmp/test.txt "s3://$BUCKET/test.txt" --endpoint-url "$ENDPOINT"
echo ""

# List objects
echo "3. Listing objects in bucket"
aws s3 ls "s3://$BUCKET/" --endpoint-url "$ENDPOINT"
echo ""

# Download file
echo "4. Downloading file"
aws s3 cp "s3://$BUCKET/test.txt" /tmp/downloaded.txt --endpoint-url "$ENDPOINT"
cat /tmp/downloaded.txt
echo ""

# Upload multiple files
echo "5. Uploading multiple files"
echo "File 1" > /tmp/file1.txt
echo "File 2" > /tmp/file2.txt
echo "File 3" > /tmp/file3.txt
aws s3 cp /tmp/file1.txt "s3://$BUCKET/files/file1.txt" --endpoint-url "$ENDPOINT"
aws s3 cp /tmp/file2.txt "s3://$BUCKET/files/file2.txt" --endpoint-url "$ENDPOINT"
aws s3 cp /tmp/file3.txt "s3://$BUCKET/files/file3.txt" --endpoint-url "$ENDPOINT"
echo ""

# List with prefix
echo "6. Listing objects with prefix 'files/'"
aws s3 ls "s3://$BUCKET/files/" --endpoint-url "$ENDPOINT"
echo ""

# Sync directory
echo "7. Syncing a directory"
mkdir -p /tmp/sync-test
echo "Sync file 1" > /tmp/sync-test/sync1.txt
echo "Sync file 2" > /tmp/sync-test/sync2.txt
aws s3 sync /tmp/sync-test "s3://$BUCKET/synced/" --endpoint-url "$ENDPOINT"
echo ""

# List all objects
echo "8. Listing all objects"
aws s3 ls "s3://$BUCKET/" --recursive --endpoint-url "$ENDPOINT"
echo ""

# Delete objects
echo "9. Cleaning up - deleting objects"
aws s3 rm "s3://$BUCKET/test.txt" --endpoint-url "$ENDPOINT"
aws s3 rm "s3://$BUCKET/" --recursive --endpoint-url "$ENDPOINT"
echo ""

# Delete bucket
echo "10. Deleting bucket"
aws s3 rb "s3://$BUCKET" --endpoint-url "$ENDPOINT"
echo ""

# Cleanup temp files
rm -f /tmp/test.txt /tmp/downloaded.txt /tmp/file*.txt
rm -rf /tmp/sync-test

echo "=== Example completed successfully! ==="

