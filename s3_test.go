package fs_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-faster/fs"
)

func TestS3Server(t *testing.T) {
	// Create temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "s3-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create S3 server
	server, err := fs.NewS3Server(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create S3 server: %v", err)
	}

	ctx := context.Background()

	t.Run("ListBuckets_Empty", func(t *testing.T) {
		buckets, err := server.ListBuckets(ctx)
		if err != nil {
			t.Fatalf("ListBuckets failed: %v", err)
		}
		if len(buckets) != 0 {
			t.Errorf("Expected 0 buckets, got %d", len(buckets))
		}
	})

	t.Run("CreateBucket", func(t *testing.T) {
		err := server.CreateBucket(ctx, "test-bucket")
		if err != nil {
			t.Fatalf("CreateBucket failed: %v", err)
		}

		// Verify bucket exists
		bucketPath := filepath.Join(tmpDir, "test-bucket")
		if _, err := os.Stat(bucketPath); os.IsNotExist(err) {
			t.Error("Bucket directory was not created")
		}
	})

	t.Run("ListBuckets_AfterCreate", func(t *testing.T) {
		buckets, err := server.ListBuckets(ctx)
		if err != nil {
			t.Fatalf("ListBuckets failed: %v", err)
		}
		if len(buckets) != 1 {
			t.Errorf("Expected 1 bucket, got %d", len(buckets))
		}
		if len(buckets) > 0 && buckets[0].Name != "test-bucket" {
			t.Errorf("Expected bucket name 'test-bucket', got '%s'", buckets[0].Name)
		}
	})

	t.Run("PutObject", func(t *testing.T) {
		content := []byte("Hello, S3!")
		reader := bytes.NewReader(content)

		err := server.PutObject(ctx, "test-bucket", "hello.txt", reader, int64(len(content)))
		if err != nil {
			t.Fatalf("PutObject failed: %v", err)
		}

		// Verify file exists
		objectPath := filepath.Join(tmpDir, "test-bucket", "hello.txt")
		if _, err := os.Stat(objectPath); os.IsNotExist(err) {
			t.Error("Object file was not created")
		}
	})

	t.Run("GetObject", func(t *testing.T) {
		rc, size, err := server.GetObject(ctx, "test-bucket", "hello.txt")
		if err != nil {
			t.Fatalf("GetObject failed: %v", err)
		}
		defer rc.Close()

		content, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("Failed to read object: %v", err)
		}

		expected := "Hello, S3!"
		if string(content) != expected {
			t.Errorf("Expected content '%s', got '%s'", expected, string(content))
		}
		if size != int64(len(expected)) {
			t.Errorf("Expected size %d, got %d", len(expected), size)
		}
	})

	t.Run("ListObjects", func(t *testing.T) {
		objects, err := server.ListObjects(ctx, "test-bucket", "")
		if err != nil {
			t.Fatalf("ListObjects failed: %v", err)
		}
		if len(objects) != 1 {
			t.Errorf("Expected 1 object, got %d", len(objects))
		}
		if len(objects) > 0 && objects[0].Key != "hello.txt" {
			t.Errorf("Expected object key 'hello.txt', got '%s'", objects[0].Key)
		}
	})

	t.Run("PutObject_WithPath", func(t *testing.T) {
		content := []byte("Nested object")
		reader := bytes.NewReader(content)

		err := server.PutObject(ctx, "test-bucket", "dir/nested.txt", reader, int64(len(content)))
		if err != nil {
			t.Fatalf("PutObject with path failed: %v", err)
		}

		// Verify file exists
		objectPath := filepath.Join(tmpDir, "test-bucket", "dir", "nested.txt")
		if _, err := os.Stat(objectPath); os.IsNotExist(err) {
			t.Error("Nested object file was not created")
		}
	})

	t.Run("ListObjects_WithPrefix", func(t *testing.T) {
		objects, err := server.ListObjects(ctx, "test-bucket", "dir/")
		if err != nil {
			t.Fatalf("ListObjects with prefix failed: %v", err)
		}
		if len(objects) != 1 {
			t.Errorf("Expected 1 object with prefix 'dir/', got %d", len(objects))
		}
		if len(objects) > 0 && objects[0].Key != "dir/nested.txt" {
			t.Errorf("Expected object key 'dir/nested.txt', got '%s'", objects[0].Key)
		}
	})

	t.Run("DeleteObject", func(t *testing.T) {
		err := server.DeleteObject(ctx, "test-bucket", "hello.txt")
		if err != nil {
			t.Fatalf("DeleteObject failed: %v", err)
		}

		// Verify file is deleted
		objectPath := filepath.Join(tmpDir, "test-bucket", "hello.txt")
		if _, err := os.Stat(objectPath); !os.IsNotExist(err) {
			t.Error("Object file was not deleted")
		}
	})

	t.Run("DeleteBucket", func(t *testing.T) {
		// Clean up remaining objects first
		server.DeleteObject(ctx, "test-bucket", "dir/nested.txt")
		os.Remove(filepath.Join(tmpDir, "test-bucket", "dir"))

		err := server.DeleteBucket(ctx, "test-bucket")
		if err != nil {
			t.Fatalf("DeleteBucket failed: %v", err)
		}

		// Verify bucket is deleted
		bucketPath := filepath.Join(tmpDir, "test-bucket")
		if _, err := os.Stat(bucketPath); !os.IsNotExist(err) {
			t.Error("Bucket directory was not deleted")
		}
	})
}
