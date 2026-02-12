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
	"gopkg.in/yaml.v3"

	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/internal/core/storage/storagefs"
)

func S3() *cobra.Command {
	var (
		configPath string
		addr       string
		root       string
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
compatible with S3 clients.

Configuration can be provided via YAML file (--config) or command-line flags.
Command-line flags override YAML configuration values.`,
		Example: `  # Start server with YAML configuration
  fs s3 --config config.yaml

  # Start server on default port (8080) with default data directory
  fs s3

  # Start server on custom port with custom data directory
  fs s3 --addr :9000 --root /data/s3

  # Use config file and override specific settings
  fs s3 --config config.yaml --addr :9000

  # Generate example configuration file
  fs s3 --generate-config > config.yaml`,
		Run: func(cmd *cobra.Command, args []string) {
			// Handle generate-config flag
			generateConfig, _ := cmd.Flags().GetBool("generate-config")
			if generateConfig {
				cfg := DefaultConfig()

				data, err := yaml.Marshal(&cfg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error generating config: %v\n", err)
					os.Exit(1)
				}

				fmt.Print(string(data))

				return
			}

			// Load configuration
			cfg, err := LoadConfig(configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
				os.Exit(1)
			}

			// Override with command-line flags if provided
			if cmd.Flags().Changed("addr") {
				cfg.Server.Addr = addr
			}

			if cmd.Flags().Changed("root") {
				cfg.Storage.Root = root
			}

			// Validate configuration
			if err := cfg.Validate(); err != nil {
				fmt.Fprintf(os.Stderr, "Error validating config: %v\n", err)
				os.Exit(1)
			}

			app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
				// Log configuration
				lg.Info("Starting with configuration",
					zap.String("addr", cfg.Server.Addr),
					zap.String("root", cfg.Storage.Root),
					zap.Duration("read_timeout", cfg.Server.ReadTimeout),
					zap.Duration("write_timeout", cfg.Server.WriteTimeout),
					zap.Duration("idle_timeout", cfg.Server.IdleTimeout),
				)

				// Make root path absolute
				absRoot, err := filepath.Abs(cfg.Storage.Root)
				if err != nil {
					return fmt.Errorf("failed to resolve root path: %w", err)
				}

				storage, err := storagefs.New(absRoot)
				if err != nil {
					return fmt.Errorf("failed to create storage: %w", err)
				}

				// Pre-create buckets if configured
				if len(cfg.Storage.Buckets) > 0 {
					lg.Info("Pre-creating buckets", zap.Strings("buckets", cfg.Storage.Buckets))

					for _, bucketName := range cfg.Storage.Buckets {
						if err := storage.CreateBucket(ctx, bucketName); err != nil {
							return fmt.Errorf("failed to create bucket %q: %w", bucketName, err)
						}

						lg.Info("Ensured bucket exists", zap.String("bucket", bucketName))
					}
				}

				svc := service.New(storage)
				h := handler.New(svc)

				// Create HTTP server
				mux := http.NewServeMux()
				mux.Handle("/", h)

				// Add health check endpoint
				mux.HandleFunc(cfg.Server.HealthPath, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)

					if _, err := fmt.Fprintf(w, "OK"); err != nil {
						// Log error but don't fail since headers already sent
						fmt.Fprintf(os.Stderr, "Health check write error: %v\n", err)
					}
				})

				// Build handler with optional request logging
				var finalHandler http.Handler = mux
				if cfg.Observability.EnableRequestLogging {
					finalHandler = loggingMiddleware(mux)
				}

				server := &http.Server{
					Addr: cfg.Server.Addr,
					Handler: otelhttp.NewHandler(finalHandler, "Operation",
						otelhttp.WithPropagators(t.TextMapPropagator()),
						otelhttp.WithMeterProvider(t.MeterProvider()),
						otelhttp.WithTracerProvider(t.TracerProvider()),
					),
					ReadTimeout:  cfg.Server.ReadTimeout,
					WriteTimeout: cfg.Server.WriteTimeout,
					IdleTimeout:  cfg.Server.IdleTimeout,
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
				app.WithServiceName(cfg.Observability.ServiceName),
			)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration file")
	cmd.Flags().StringVar(&addr, "addr", ":8080", "Address to listen on (overrides config file)")
	cmd.Flags().StringVar(&root, "root", ".s3data", "Root directory for S3 storage (overrides config file)")
	cmd.Flags().Bool("generate-config", false, "Generate example configuration file and print to stdout")

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
