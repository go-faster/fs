package main

import (
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// The peer transport carries an S3 request across a node boundary, and until
// both halves below are instrumented that boundary is where observability
// stops: the coordinator records "peer returned status 500" against its trace,
// the peer records its cause against a trace of its own, and nothing joins the
// two. Wrapping the client injects the trace context, wrapping the listener
// extracts it — after which the peer's span is a child of the S3 request's, and
// zctx tags the peer's log lines with the same trace_id.
//
// This complements the request ID (internal/reqid), which survives where traces
// do not: it is in the error the client was handed, so it works from a bug
// report with no collector involved.
//
// Providers and the propagator come from the OpenTelemetry globals, which
// app.Run installs before any of this is built. A process running without
// telemetry gets no-ops rather than a nil dereference.

// peerHTTPClient returns the HTTP client peer calls ride on. The wrapped
// transport injects the caller's trace context into every outgoing request. A
// zero timeout means none, matching http.DefaultClient.
func peerHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
}

// instrumentPeerHandler wraps the peer listener so an incoming call joins the
// trace of the S3 request that caused it.
func instrumentPeerHandler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "Peer", otelhttp.WithSpanNameFormatter(peerSpanName))
}

// peerSpanName names a peer span by method and endpoint, stopping before the
// disk and fragment name. Those are in the path, and one span name per fragment
// is unusable cardinality — the fragment identity belongs in attributes.
func peerSpanName(_ string, r *http.Request) string {
	route := r.URL.Path

	if parts := strings.SplitN(strings.TrimPrefix(route, "/"), "/", 3); len(parts) >= 2 {
		route = "/" + parts[0] + "/" + parts[1]
	}

	return r.Method + " " + route
}
