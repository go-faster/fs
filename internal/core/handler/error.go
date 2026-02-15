package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/go-faster/fs"
)

type Error struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Message string `json:"message"`
}

func newError(ctx context.Context, err error) Error {
	if err == nil {
		err = errors.New("internal error")
	}

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

	return e
}

func httpStatusFromError(err error) int {
	switch {
	case errors.Is(err, fs.ErrBucketNotFound):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrObjectNotFound):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrUploadNotFound):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrInvalidBucketName):
		return http.StatusBadRequest
	case errors.Is(err, fs.ErrUnsupportedOperation):
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
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
	w.WriteHeader(httpStatusFromError(err))

	if _, writeErr := w.Write(data); writeErr != nil {
		zctx.From(ctx).Error("Failed to write error response",
			zap.Error(writeErr),
		)
	}
}
