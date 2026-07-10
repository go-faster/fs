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
	"gopkg.in/yaml.v3"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagefs"
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

				// wrap injects OpenTelemetry instrumentation and optional request
				// logging into the embeddable server's handler.
				wrap := func(h http.Handler) http.Handler {
					if cfg.Observability.EnableRequestLogging {
						h = loggingMiddleware(h)
					}

					return otelhttp.NewHandler(h, "Operation",
						otelhttp.WithPropagators(t.TextMapPropagator()),
						otelhttp.WithMeterProvider(t.MeterProvider()),
						otelhttp.WithTracerProvider(t.TracerProvider()),
					)
				}

				srv, err := server.New(server.Config{
					Storage:      storage,
					Addr:         cfg.Server.Addr,
					ReadTimeout:  cfg.Server.ReadTimeout,
					WriteTimeout: cfg.Server.WriteTimeout,
					IdleTimeout:  cfg.Server.IdleTimeout,
					HealthPath:   cfg.Server.HealthPath,
					Buckets:      cfg.Storage.Buckets,
					WrapHandler:  wrap,
				})
				if err != nil {
					return errors.Wrap(err, "create server")
				}

				// NB: Explicitly using t.BaseContext() for new connections so that
				// telemetry is properly tied to the application lifecycle.
				srv.HTTPServer().ConnContext = func(context.Context, net.Conn) context.Context {
					return t.BaseContext()
				}

				lg.Info("Starting server", zap.String("addr", cfg.Server.Addr))

				// NB: Using ShutdownContext is important to properly execute graceful
				// shutdown: ListenAndServe serves until it is canceled, then drains
				// in-flight requests using a detached context.
				if err := srv.ListenAndServe(t.ShutdownContext()); err != nil {
					return errors.Wrap(err, "listen and serve")
				}

				return nil
			},
				app.WithServiceName(cfg.Observability.ServiceName),
			)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration file")
	cmd.Flags().StringVar(&addr, "addr", server.DefaultAddr, "Address to listen on (overrides config file)")
	cmd.Flags().StringVar(&root, "root", DefaultStorageRoot, "Root directory for S3 storage (overrides config file)")
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
