package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestPeerSpanName(t *testing.T) {
	for name, tc := range map[string]struct{ method, path, want string }{
		"fragment": {http.MethodPut, "/v1/fragments/d0/bucket/key/0", "PUT /v1/fragments"},
		"names":    {http.MethodGet, "/v1/names/d0/bucket/key", "GET /v1/names"},
		"status":   {http.MethodGet, "/v1/status", "GET /v1/status"},
		"index":    {http.MethodGet, "/v1/index", "GET /v1/index"},
		"short":    {http.MethodGet, "/", "GET /"},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
			assert.Equal(t, tc.want, peerSpanName("", req))
		})
	}
}

// TestPeerSpanNameIsBounded is the property the formatter exists for: two
// requests differing only in disk and fragment must not produce two span names.
func TestPeerSpanNameIsBounded(t *testing.T) {
	a := httptest.NewRequest(http.MethodPut, "/v1/fragments/d0/bucket/key-a/0", http.NoBody)
	b := httptest.NewRequest(http.MethodPut, "/v1/fragments/d7/other/key-b/3", http.NoBody)

	assert.Equal(t, peerSpanName("", a), peerSpanName("", b))
}

// TestTraceContextCrossesPeerHop pins the reason both halves are wrapped: a
// peer call must land in the trace of the S3 request that caused it, so the
// coordinator's "peer returned status 500" and the peer's cause share a
// trace_id instead of being two unrelated roots.
func TestTraceContextCrossesPeerHop(t *testing.T) {
	tp := sdktrace.NewTracerProvider()

	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	// otelhttp reads the globals; app.Run installs them in production.
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	peerSide := make(chan trace.SpanContext, 1)
	srv := httptest.NewServer(instrumentPeerHandler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		peerSide <- trace.SpanContextFromContext(r.Context())
	})))

	t.Cleanup(srv.Close)

	ctx, span := tp.Tracer("test").Start(context.Background(), "PutObject")

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		srv.URL+"/v1/fragments/d0/bucket/key/0", http.NoBody)
	require.NoError(t, err)

	resp, err := peerHTTPClient(0).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	span.End()

	got := <-peerSide
	require.True(t, got.IsValid(), "peer request must carry a span context")
	assert.Equal(t, span.SpanContext().TraceID(), got.TraceID(),
		"peer span must join the caller's trace")
	assert.NotEqual(t, span.SpanContext().SpanID(), got.SpanID(),
		"peer span must be its own span, not the caller's")
}
