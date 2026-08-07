package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-faster/sdk/zctx"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/storagemem"
)

// observedHandler returns a handler whose failures land in logs, with one
// bucket ("bucket") already created.
func observedHandler(t testing.TB) (http.Handler, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zapcore.DebugLevel)
	storage := storagemem.New()

	require.NoError(t, storage.CreateBucket(t.Context(), "bucket"))

	h := handler.New(service.New(storage))

	// The handler logs through zctx, so the observer has to be the context's
	// base logger.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(zctx.Base(r.Context(), zap.New(core))))
	}), logs
}

func request(t testing.TB, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// TestNotFoundLogsAtDebug is the noise guard.
//
// A missing bucket or key is an ordinary client outcome — a probe, a retry, a
// GET of something already deleted — and logging it at error makes the loudest
// thing in a healthy server's log a request that behaved exactly as S3 says it
// should.
func TestNotFoundLogsAtDebug(t *testing.T) {
	for name, tc := range map[string]struct {
		method, target string
		code           string
		bucket, key    string
	}{
		"NoSuchBucket": {
			method: http.MethodGet, target: "/absent?list-type=2",
			code: "NoSuchBucket", bucket: "absent",
		},
		"NoSuchBucketOnObject": {
			method: http.MethodGet, target: "/absent/some/key.txt",
			code: "NoSuchBucket", bucket: "absent", key: "some/key.txt",
		},
		"NoSuchKey": {
			method: http.MethodGet, target: "/bucket/missing.txt",
			code: "NoSuchKey", bucket: "bucket", key: "missing.txt",
		},
		"NoSuchKeyOnHead": {
			method: http.MethodHead, target: "/bucket/missing.txt",
			code: "NoSuchKey", bucket: "bucket", key: "missing.txt",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, logs := observedHandler(t)

			require.Equal(t, http.StatusNotFound, request(t, h, tc.method, tc.target).Code)

			entries := logs.FilterMessage("Request failed").All()
			require.Len(t, entries, 1)

			e := entries[0]
			require.Equal(t, zapcore.DebugLevel, e.Level, "a missing %s must not log at error", tc.code)

			fields := e.ContextMap()
			require.Equal(t, tc.code, fields["code"])
			require.Equal(t, tc.bucket, fields["bucket"])

			if tc.key == "" {
				require.NotContains(t, fields, "key", "a bucket-level request has no key to report")
			} else {
				require.Equal(t, tc.key, fields["key"])
			}
		})
	}
}

// TestOtherErrorsStillLogAtError: only the two not-found codes are demoted. A
// real fault must stay loud, or this change trades one kind of blindness for
// another.
func TestOtherErrorsStillLogAtError(t *testing.T) {
	h, logs := observedHandler(t)

	// A malformed lifecycle document: a client error, but not a missing name.
	req := httptest.NewRequest(http.MethodPut, "/bucket?lifecycle", strings.NewReader("<not-xml"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)

	entries := logs.FilterMessage("Request failed").All()
	require.Len(t, entries, 1)
	require.Equal(t, zapcore.ErrorLevel, entries[0].Level)
	require.Equal(t, "bucket", entries[0].ContextMap()["bucket"])
}
