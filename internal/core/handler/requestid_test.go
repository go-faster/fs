package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/reqid"
)

// TestRequestIDIsAlwaysMinted pins the property that keeps a crafted ID from
// ever entering the system: the edge generates its own and ignores whatever the
// client sent. A client-chosen ID would be a value we log on every line of a
// request and forward to every peer — the one place it must not come from.
func TestRequestIDIsAlwaysMinted(t *testing.T) {
	h := newStorageHandler(t)

	for name, sent := range map[string]string{
		"none":      "",
		"short":     "deadbeef",
		"oversized": strings.Repeat("A", 64*1024),
		"xmlBreak":  "</RequestId><script>",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			if sent != "" {
				req.Header.Set(reqid.Header, sent)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(reqid.Header)
			require.NotEmpty(t, got)
			assert.True(t, reqid.Valid(got), "minted ID %q must be valid", got)

			if sent != "" {
				assert.NotEqual(t, sent, got, "client value must never be echoed")
				// The error body renders <RequestId> from the same header.
				assert.NotContains(t, rec.Body.String(), sent)
			}
		})
	}
}

// TestRequestIDIsUniquePerRequest keeps the ID usable as a correlation key.
func TestRequestIDIsUniquePerRequest(t *testing.T) {
	h := newStorageHandler(t)
	seen := make(map[string]bool, 32)

	for range 32 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))

		id := rec.Header().Get(reqid.Header)
		assert.False(t, seen[id], "duplicate request ID %q", id)
		seen[id] = true
	}
}
