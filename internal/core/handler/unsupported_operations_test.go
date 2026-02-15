package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/handler"
)

func TestHandler_UnsupportedOperations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		method         string
		path           string
		query          string
		expectedStatus int
	}{
		{
			name:           "POST to bucket without query params",
			method:         "POST",
			path:           "/test-bucket",
			query:          "",
			expectedStatus: http.StatusNotImplemented,
		},
		{
			name:           "POST to bucket with unknown query param",
			method:         "POST",
			path:           "/test-bucket",
			query:          "unknown=value",
			expectedStatus: http.StatusNotImplemented,
		},
		{
			name:           "POST to object without query params",
			method:         "POST",
			path:           "/test-bucket/test-key",
			query:          "",
			expectedStatus: http.StatusNotImplemented,
		},
		{
			name:           "POST to object with unknown query param",
			method:         "POST",
			path:           "/test-bucket/test-key",
			query:          "unknown=value",
			expectedStatus: http.StatusNotImplemented,
		},
	}

	svc := baseMock()
	h := handler.New(svc)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := tt.path
			if tt.query != "" {
				url += "?" + tt.query
			}

			req := httptest.NewRequest(tt.method, url, nil)
			req = req.WithContext(context.Background())
			w := httptest.NewRecorder()

			h.ServeHTTP(w, req)

			require.Equal(t, tt.expectedStatus, w.Code,
				"Expected status %d (%s), got %d for %s %s",
				tt.expectedStatus, http.StatusText(tt.expectedStatus),
				w.Code, tt.method, url)

			// Verify response contains error message.
			body := w.Body.String()
			require.Contains(t, strings.ToLower(body), "unsupported",
				"Response should contain 'unsupported' in error message")
		})
	}
}

func TestHandler_SupportedOperations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		method         string
		path           string
		query          string
		expectedStatus int
		setupMock      func(svc *fs.Service)
	}{
		{
			name:           "POST to bucket with delete query",
			method:         "POST",
			path:           "/test-bucket",
			query:          "delete",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "POST to object with uploads query",
			method:         "POST",
			path:           "/test-bucket/test-key",
			query:          "uploads",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "POST to object with uploadId query",
			method:         "POST",
			path:           "/test-bucket/test-key",
			query:          "uploadId=test-upload-123",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := baseMock()

			// Setup specific mocks for supported operations.
			svc.CreateMultipartUploadFunc = func(ctx context.Context, bucket, key string) (*fs.MultipartUpload, error) {
				return &fs.MultipartUpload{
					UploadID: "test-123",
					Bucket:   bucket,
					Key:      key,
				}, nil
			}
			svc.CompleteMultipartUploadFunc = func(ctx context.Context, req *fs.CompleteMultipartUploadRequest) (*fs.CompleteMultipartUploadResponse, error) {
				return &fs.CompleteMultipartUploadResponse{
					Location: "/" + req.Bucket + "/" + req.Key,
					Bucket:   req.Bucket,
					Key:      req.Key,
					ETag:     "test-etag",
				}, nil
			}

			h := handler.New(svc)

			url := tt.path
			if tt.query != "" {
				url += "?" + tt.query
			}

			var body string
			if strings.Contains(tt.query, "uploadId") {
				// CompleteMultipartUpload requires XML body.
				body = `<CompleteMultipartUpload></CompleteMultipartUpload>`
			} else if strings.Contains(tt.query, "delete") {
				// DeleteObjects requires XML body.
				body = `<Delete></Delete>`
			}

			req := httptest.NewRequest(tt.method, url, strings.NewReader(body))
			req = req.WithContext(context.Background())
			w := httptest.NewRecorder()

			h.ServeHTTP(w, req)

			require.Equal(t, tt.expectedStatus, w.Code,
				"Expected status %d, got %d for %s %s",
				tt.expectedStatus, w.Code, tt.method, url)
		})
	}
}

