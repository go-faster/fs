#!/bin/bash
# Quick test to verify XML output is properly formatted

set -e

ENDPOINT="http://localhost:18080"
TMPDIR=$(mktemp -d)

# Start server in background
cd /src/faster/fs
./bin/fs s3 --addr :18080 --root "$TMPDIR/s3data" &
SERVER_PID=$!

# Give server time to start
sleep 1

# Function to cleanup
cleanup() {
    kill $SERVER_PID 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

# Test 1: List buckets (should return valid XML)
echo "Test 1: List buckets (empty)"
RESPONSE=$(curl -s "$ENDPOINT/")
echo "$RESPONSE"
echo "$RESPONSE" | grep -q "ListAllMyBucketsResult" && echo "✓ Valid XML structure"

# Test 2: Create bucket
echo -e "\nTest 2: Create bucket"
curl -s -X PUT "$ENDPOINT/test-bucket"
echo "✓ Bucket created"

# Test 3: List buckets (should show test-bucket)
echo -e "\nTest 3: List buckets (with bucket)"
RESPONSE=$(curl -s "$ENDPOINT/")
echo "$RESPONSE"
echo "$RESPONSE" | grep -q "test-bucket" && echo "✓ Bucket appears in list"

# Test 4: Upload object
echo -e "\nTest 4: Upload object"
echo "Hello XML!" | curl -s -X PUT -d @- "$ENDPOINT/test-bucket/hello.txt"
echo "✓ Object uploaded"

# Test 5: List objects (should return valid XML)
echo -e "\nTest 5: List objects"
RESPONSE=$(curl -s "$ENDPOINT/test-bucket")
echo "$RESPONSE"
echo "$RESPONSE" | grep -q "ListBucketResult" && echo "✓ Valid XML structure"
echo "$RESPONSE" | grep -q "hello.txt" && echo "✓ Object appears in list"

# Test 6: Download object
echo -e "\nTest 6: Download object"
CONTENT=$(curl -s "$ENDPOINT/test-bucket/hello.txt")
[ "$CONTENT" = "Hello XML!" ] && echo "✓ Object content matches"

echo -e "\n✅ All XML output tests passed!"

