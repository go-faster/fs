package fs_test

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-faster/fs"
)

func TestXMLOutput(t *testing.T) {
	// Create temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "s3-xml-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create S3 server
	server, err := fs.NewS3Server(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create S3 server: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	t.Run("ListBuckets_XML_Valid", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET / failed: %v", err)
		}
		defer resp.Body.Close()

		// Parse the XML to ensure it's well-formed
		var result fs.ListAllMyBucketsResult
		if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Errorf("Failed to parse XML response: %v", err)
		}

		// Check content type
		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "application/xml") {
			t.Errorf("Expected Content-Type to contain 'application/xml', got '%s'", contentType)
		}
	})

	t.Run("ListObjects_XML_Valid", func(t *testing.T) {
		// Create a bucket first
		req := httptest.NewRequest("PUT", "/test-bucket", http.NoBody)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		// List objects
		resp, err := ts.Client().Get(ts.URL + "/test-bucket")
		if err != nil {
			t.Fatalf("GET /test-bucket failed: %v", err)
		}
		defer resp.Body.Close()

		// Parse the XML to ensure it's well-formed
		var result fs.ListBucketResult
		if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Errorf("Failed to parse XML response: %v", err)
		}

		if result.Name != "test-bucket" {
			t.Errorf("Expected bucket name 'test-bucket', got '%s'", result.Name)
		}

		// Check content type
		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "application/xml") {
			t.Errorf("Expected Content-Type to contain 'application/xml', got '%s'", contentType)
		}
	})

	t.Run("XML_Namespace", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET / failed: %v", err)
		}
		defer resp.Body.Close()

		var result fs.ListAllMyBucketsResult
		if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Errorf("Failed to parse XML response: %v", err)
		}

		// Verify the namespace is correct
		expectedNamespace := "http://s3.amazonaws.com/doc/2006-03-01/"
		if result.XMLName.Space != expectedNamespace {
			t.Errorf("Expected namespace '%s', got '%s'", expectedNamespace, result.XMLName.Space)
		}
	})
}
