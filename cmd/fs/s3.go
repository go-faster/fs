package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"github.com/go-faster/sdk/zctx"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/internal/core/storage/storagefs"
)

func S3() *cobra.Command {
	var (
		addr string
		root string
	)

	cmd := &cobra.Command{
		Use:   "s3",
		Short: "Start S3-compatible storage server",
		Long: `Start an S3-compatible storage server.

This command starts a lightweight S3-compatible storage server that implements
basic S3 API operations including:
  - Bucket operations (create, delete, list)
  - Object operations (put, get, delete, list)

The server stores data in a local directory and provides an HTTP interface
compatible with S3 clients.`,
		Example: `  # Start server on default port (8080) with default data directory
  fs s3

  # Start server on custom port with custom data directory
  fs s3 --addr :9000 --root /data/s3

  # Start server and bind to specific interface
  fs s3 --addr 127.0.0.1:8080`,
		Run: func(cmd *cobra.Command, args []string) {
			app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
				// Make root path absolute
				absRoot, err := filepath.Abs(root)
				if err != nil {
					return fmt.Errorf("failed to resolve root path: %w", err)
				}

				storage, err := storagefs.New(absRoot)
				if err != nil {
					return fmt.Errorf("failed to create storage: %w", err)
				}

				svc := service.New(storage)
				h := handler.New(svc)

				// Create HTTP server
				mux := http.NewServeMux()
				mux.Handle("/", h)

				// Add health check endpoint
				mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)

					if _, err := fmt.Fprintf(w, "OK"); err != nil {
						// Log error but don't fail since headers already sent
						fmt.Fprintf(os.Stderr, "Health check write error: %v\n", err)
					}
				})

				server := &http.Server{
					Addr: addr,
					Handler: otelhttp.NewHandler(loggingMiddleware(mux), "Operation",
						otelhttp.WithPropagators(t.TextMapPropagator()),
						otelhttp.WithMeterProvider(t.MeterProvider()),
						otelhttp.WithTracerProvider(t.TracerProvider()),
					),
					ReadTimeout:  30 * time.Second,
					WriteTimeout: 30 * time.Second,
					IdleTimeout:  120 * time.Second,
					ConnContext: func(ctx context.Context, c net.Conn) context.Context {
						return t.BaseContext()
					},
				}

				g, gCtx := errgroup.WithContext(ctx)
				g.Go(func() error {
					lg.Info("Starting server", zap.String("addr", server.Addr))

					if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
						return errors.Wrap(err, "listen and serve")
					}

					return nil
				})
				g.Go(func() error {
					// NB: Using ShutdownContext is important to properly execute graceful shutdown.
					shutdownContext := t.ShutdownContext()
					select {
					case <-gCtx.Done():
						// Non-graceful shutdown.
						lg.Warn("Context done before shutdown")
					case <-shutdownContext.Done():
						lg.Info("Shutting down server")
					}
					// NB: Explicitly using t.BaseContext() to ensure that server
					// is properly shut down before application exits.
					//
					// This context is canceled when shutdown is completed.
					return server.Shutdown(t.BaseContext())
				})

				return g.Wait()
			},
				app.WithServiceName("go-faster/fs"),
			)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":8080", "Address to listen on")
	cmd.Flags().StringVar(&root, "root", ".s3data", "Root directory for S3 storage")

	return cmd
}

// loggingMiddleware logs HTTP requests
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a response writer wrapper to capture status code
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(ww, r)

		duration := time.Since(start)

		zctx.From(r.Context()).Info(r.Method,
			zap.String("path", r.URL.Path),
			zap.Int("status", ww.statusCode),
			zap.Duration("duration", duration),
			zap.String("remote_addr", r.RemoteAddr),
			zap.String("user_agent", r.UserAgent()),
		)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
