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

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagefs"
)

func S3() *cobra.Command {
	var (
		configPath string
		addr       string
		root       string
		tlsCert    string
		tlsKey     string
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

			if cmd.Flags().Changed("tls-cert") {
				cfg.Server.TLS.CertFile = tlsCert
			}

			if cmd.Flags().Changed("tls-key") {
				cfg.Server.TLS.KeyFile = tlsKey
			}

			// Validate configuration
			if err := cfg.Validate(); err != nil {
				fmt.Fprintf(os.Stderr, "Error validating config: %v\n", err)
				os.Exit(1)
			}

			insecureNoAuth, _ := cmd.Flags().GetBool("insecure-no-auth")

			startTime := time.Now()

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

				// The auth manager owns the credential store and adds runtime
				// access-key management (used by the admin API); it persists
				// runtime-created keys under the storage root.
				authManager, err := buildAuthManager(cfg, insecureNoAuth, resolveAdminKeysFile(cfg, absRoot))
				if err != nil {
					return errors.Wrap(err, "configure auth")
				}

				var authStore *auth.Store
				if authManager != nil {
					authStore = authManager.Store()
				}

				var (
					storage   fs.Storage
					clusterRT *clusterRuntime
				)

				switch cfg.Storage.Type {
				case StorageTypeCluster:
					clusterRT, err = buildCluster(ctx, lg, cfg, absRoot)
					if err != nil {
						return errors.Wrap(err, "cluster mode")
					}

					storage = clusterRT.Storage

					// Cluster-wide scrub/repair on the single-node scrub cadence.
					go clusterRT.RunScrubber(ctx, cfg.Integrity.ScrubInterval)

					// Auto rebalancing: converge placement after settled
					// membership changes without operator action.
					go clusterRT.RunAutoRebalancer(ctx, cfg.Cluster.Rebalance)

					// Per-disk capacity into the registry + fullness watermark
					// warnings; cluster metrics for the telemetry pipeline.
					go clusterRT.RunUsageReporter(ctx, cfg.Cluster.Rebalance.FullWatermark)

					if err := clusterRT.RegisterMetrics(t.MeterProvider()); err != nil {
						return errors.Wrap(err, "register cluster metrics")
					}
				default: // StorageTypeFilesystem, enforced by Validate.
					syncPolicy, err := storagefs.ParseSyncPolicy(cfg.Storage.Fsync)
					if err != nil {
						return errors.Wrap(err, "storage fsync policy")
					}

					fsStorage, err := storagefs.New(absRoot,
						storagefs.WithSyncPolicy(syncPolicy),
						storagefs.WithVerifyReads(cfg.Integrity.VerifyOnRead),
					)
					if err != nil {
						return fmt.Errorf("failed to create storage: %w", err)
					}

					storage = fsStorage

					// Background integrity scrubber (no-op unless an interval is
					// set). Cluster-mode scrub/repair is the Phase 8 repair worker.
					go runScrubber(ctx, lg, fsStorage, cfg.Integrity)
				}

				lg.Info("Durability",
					zap.String("fsync", cfg.Storage.Fsync),
					zap.Bool("verify_on_read", cfg.Integrity.VerifyOnRead),
					zap.String("storage_type", cfg.Storage.Type),
				)

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

				serverCfg := server.Config{
					Storage:      storage,
					Addr:         cfg.Server.Addr,
					ReadTimeout:  cfg.Server.ReadTimeout,
					WriteTimeout: cfg.Server.WriteTimeout,
					IdleTimeout:  cfg.Server.IdleTimeout,
					HealthPath:   cfg.Server.HealthPath,
					Buckets:      cfg.Storage.Buckets,
					Auth:         authStore,
					WrapHandler:  wrap,
					// Readiness probes storage reachability (health is liveness only).
					Ready: func(ctx context.Context) error {
						_, err := storage.ListBuckets(ctx)
						return err
					},
				}

				if cfg.Server.TLS.CertFile != "" && cfg.Server.TLS.KeyFile != "" {
					serverCfg.TLS = &server.TLSConfig{
						CertFile: cfg.Server.TLS.CertFile,
						KeyFile:  cfg.Server.TLS.KeyFile,
					}
				}

				lg.Info("Security",
					zap.Bool("auth_enabled", authStore != nil),
					zap.Bool("tls_enabled", serverCfg.TLS != nil),
				)

				srv, err := server.New(serverCfg)
				if err != nil {
					return errors.Wrap(err, "create server")
				}

				// NB: Explicitly using t.BaseContext() for new connections so that
				// telemetry is properly tied to the application lifecycle.
				srv.HTTPServer().ConnContext = func(context.Context, net.Conn) context.Context {
					return t.BaseContext()
				}

				// Hot-reload credentials and TLS certificate on SIGHUP.
				go handleReload(ctx, lg, configPath, insecureNoAuth, authManager, srv)

				lg.Info("Starting server", zap.String("addr", cfg.Server.Addr))

				// Run the S3 server and, when enabled, the admin API + dashboard
				// on its own listener. A failure in either cancels the group.
				grp, grpCtx := errgroup.WithContext(t.ShutdownContext())

				grp.Go(func() error {
					// NB: Using the group context (from ShutdownContext) is important
					// to properly execute graceful shutdown: ListenAndServe serves
					// until it is canceled, then drains in-flight requests.
					if err := srv.ListenAndServe(grpCtx); err != nil {
						return errors.Wrap(err, "listen and serve")
					}

					return nil
				})

				if clusterRT != nil {
					grp.Go(func() error {
						return clusterRT.Serve(grpCtx)
					})
				}

				if cfg.Admin.Enabled {
					if authManager == nil {
						return errors.New("admin API requires authentication; remove --insecure-no-auth / auth.disabled or disable admin")
					}

					// Avoid a non-nil interface around a nil controller outside
					// cluster mode.
					var rebalance adminhandler.RebalanceControl
					if clusterRT != nil {
						rebalance = clusterRT.rebalance
					}

					grp.Go(func() error {
						return runAdminServer(grpCtx, lg, t, cfg.Admin, authManager, authStore != nil, startTime, rebalance)
					})
				}

				return grp.Wait()
			},
				app.WithServiceName(cfg.Observability.ServiceName),
			)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration file")
	cmd.Flags().StringVar(&addr, "addr", server.DefaultAddr, "Address to listen on (overrides config file)")
	cmd.Flags().StringVar(&root, "root", DefaultStorageRoot, "Root directory for S3 storage (overrides config file)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "Path to the TLS certificate (enables HTTPS with --tls-key)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "Path to the TLS private key (enables HTTPS with --tls-cert)")
	cmd.Flags().Bool("insecure-no-auth", false, "Disable authentication and serve anonymously (insecure)")
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
