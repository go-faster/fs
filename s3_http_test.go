package fs_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-faster/fs"
)

func TestS3ServerHTTP(t *testing.T) {
	// Create temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "s3-http-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create S3 server
	server, err := fs.NewS3Server(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create S3 server: %v", err)
	}

	// Create HTTP test server
	ts := httptest.NewServer(server)
	defer ts.Close()

	t.Run("ListBuckets_Empty", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET / failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "ListAllMyBucketsResult") {
			t.Error("Response doesn't contain expected XML")
		}
	})

	t.Run("CreateBucket", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/test-bucket", http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT /test-bucket failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}
	})

	t.Run("PutObject", func(t *testing.T) {
		content := bytes.NewReader([]byte("Hello, HTTP!"))
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/test-bucket/hello.txt", content)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT /test-bucket/hello.txt failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}
	})

	t.Run("GetObject", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/test-bucket/hello.txt")
		if err != nil {
			t.Fatalf("GET /test-bucket/hello.txt failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		expected := "Hello, HTTP!"
		if string(body) != expected {
			t.Errorf("Expected content '%s', got '%s'", expected, string(body))
		}
	})

	t.Run("ListObjects", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/test-bucket")
		if err != nil {
			t.Fatalf("GET /test-bucket failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "ListBucketResult") {
			t.Error("Response doesn't contain ListBucketResult")
		}
		if !strings.Contains(bodyStr, "hello.txt") {
			t.Error("Response doesn't contain hello.txt")
		}
	})

	t.Run("DeleteObject", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/test-bucket/hello.txt", http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE /test-bucket/hello.txt failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("Expected status 204, got %d", resp.StatusCode)
		}
	})

	t.Run("DeleteBucket", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/test-bucket", http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE /test-bucket failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("Expected status 204, got %d", resp.StatusCode)
		}
	})

	t.Run("HealthCheck", func(t *testing.T) {
		// Note: The health check endpoint is added in the CLI wrapper,
		// not in the S3Server itself, so this test would need to be
		// in the cmd package to test it properly.
		// This is just a placeholder to document the expected behavior.
		t.Skip("Health check endpoint is in CLI wrapper")
	})
}
