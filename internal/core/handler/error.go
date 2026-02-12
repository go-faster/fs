package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-faster/sdk/zctx"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

type Error struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Message string `json:"message"`
}

func newError(ctx context.Context, err error) error {
	e := Error{
		Message: err.Error(),
	}
	if span := trace.SpanFromContext(ctx).SpanContext(); span.HasTraceID() {
		// Extract trace/span IDs from context if available.
		e.TraceID = span.TraceID().String()
		e.SpanID = span.SpanID().String()
	}

	zctx.From(ctx).Error("Operation failed",
		zap.Error(err),
	)

	return err
}

func renderError(ctx context.Context, w http.ResponseWriter, err error) {
	zctx.From(ctx).Error("Failed",
		zap.Error(err),
	)

	data, marshalErr := json.Marshal(newError(ctx, err))
	if marshalErr != nil {
		zctx.From(ctx).Error("Failed to marshal error response",
			zap.Error(marshalErr),
		)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)

	if _, writeErr := w.Write(data); writeErr != nil {
		zctx.From(ctx).Error("Failed to write error response",
			zap.Error(writeErr),
		)
	}
}
