package handler_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/storagemem"
)

// newStorageHandler returns a handler backed by real in-memory storage, so tests
// exercise the full data path (ETag, seekable reader, listing).
func newStorageHandler(t testing.TB) http.Handler {
	t.Helper()

	return handler.New(service.New(storagemem.New()))
}

// do issues a request to h and returns the recorder.
func do(t testing.TB, h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var r io.Reader = http.NoBody
	if method == http.MethodPut || method == http.MethodPost {
		r = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, target, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}
