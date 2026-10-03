package main

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
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
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/lastrun"
	"github.com/go-faster/fs/server"
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

			// app.Run triggers graceful shutdown on SIGINT only, but process
			// managers (systemd, Kubernetes, docker stop) send SIGTERM. Bridge
			// SIGTERM to SIGINT so an operator stop — or a rolling upgrade —
			// drains in-flight requests and deregisters the node cleanly
			// instead of being abruptly terminated.
			bridgeSIGTERM()

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

				_, authEnabled, err := buildAuthConfig(cfg, insecureNoAuth)
				if err != nil {
					return errors.Wrap(err, "configure auth")
				}

				keyring, err := cfg.Encryption.Keyring()
				if err != nil {
					return errors.Wrap(err, "server-side encryption")
				}

				// Built before the storage: the engine registers its peer
				// endpoints on it, and it is served once they are.
				member, err := newClusterMember(cfg, absRoot)
				if err != nil {
					return err
				}

				// When each periodic pass last completed. On a single node
				// that lives in the data directory: there is no control plane
				// to ask, and the data directory is the one thing that
				// outlives the process.
				state := lastrun.NewFile(absRoot)

				eng, err := buildEngine(absRoot, member, keyring, cfg.Storage.Fsync == fsyncNone)
				if err != nil {
					return errors.Wrap(err, "storage engine")
				}

				defer func() { _ = eng.Close() }()

				if err := registerEngineMetrics(t.MeterProvider(), eng); err != nil {
					return err
				}

				var storage fs.Storage = eng

				// Bucket lifecycle rules: the sweep that makes a stored expiry
				// rule actually delete something.
				//
				// ponytail: in a cluster every node sweeps; deletes are
				// idempotent, so the cost is duplicated listing. Elect one
				// sweeper if that shows.
				go runLifecycle(ctx, lg, storage, cfg.Lifecycle, state)

				lg.Info("Durability", zap.String("fsync", cmp.Or(cfg.Storage.Fsync, fsyncFile)))

				// Whether bodies are encrypted is the kind of thing an operator
				// must be able to confirm from the log rather than infer.
				lg.Info("Encryption",
					zap.Bool("at_rest", cfg.Encryption.Enabled()),
					zap.String("default_algorithm", cfg.Encryption.DefaultAlgorithm),
					zap.Int("retired_keys", len(cfg.Encryption.PreviousKeyFiles)),
				)

				var (
					authStore   *auth.Store
					authManager *auth.Manager
				)

				if authEnabled {
					authManager, err = buildAuthManager(cfg, insecureNoAuth, resolveAdminKeysFile(cfg, absRoot))
					if err != nil {
						return errors.Wrap(err, "configure auth")
					}

					authStore = authManager.Store()
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

				serverCfg := server.Config{
					Storage:        storage,
					Addr:           cfg.Server.Addr,
					ReadTimeout:    cfg.Server.ReadTimeout,
					WriteTimeout:   cfg.Server.WriteTimeout,
					IdleTimeout:    cfg.Server.IdleTimeout,
					HealthPath:     cfg.Server.HealthPath,
					Region:         cfg.Server.Region,
					OwnerIsolation: cfg.Auth.OwnerIsolation,

					DefaultEncryption: cfg.Encryption.DefaultAlgorithm,
					Buckets:           cfg.Storage.Buckets,
					Auth:              authStore,
					WrapHandler:       wrap,
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

				// Hot-reload credentials and TLS certificate on SIGHUP or via
				// the admin reload endpoint — one reloader backs both.
				rel := newReloader(lg, configPath, insecureNoAuth, authManager, srv)
				go handleReload(ctx, rel)

				lg.Info("Starting server", zap.String("addr", cfg.Server.Addr))

				// Run the S3 server and, when enabled, the peer and admin
				// listeners. A failure in any cancels the group.
				grp, grpCtx := errgroup.WithContext(t.ShutdownContext())

				if member != nil {
					if err := serveCluster(grpCtx, lg, cfg, member, t.MeterProvider(), grp.Go); err != nil {
						return err
					}
				}

				grp.Go(func() error {
					eng.Run(grpCtx, engine.RunConfig{Cluster: member != nil})

					return nil
				})

				grp.Go(func() error {
					// NB: Using the group context (from ShutdownContext) is important
					// to properly execute graceful shutdown: ListenAndServe serves
					// until it is canceled, then drains in-flight requests.
					if err := srv.ListenAndServe(grpCtx); err != nil {
						return errors.Wrap(err, "listen and serve")
					}

					return nil
				})

				if cfg.Admin.Enabled {
					if authStore == nil {
						return errors.New("admin API requires authentication; remove --insecure-no-auth / auth.disabled or disable admin")
					}

					adminCfg := adminServerConfig{
						Admin:       cfg.Admin,
						Credentials: authManager,
						AuthEnabled: authStore != nil,
						StartTime:   startTime,
						Reloader:    rel,
						Cluster:     member,
						Engine:      eng,
					}

					grp.Go(func() error {
						return runAdminServer(grpCtx, lg, t, adminCfg)
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

// bridgeSIGTERM converts the first SIGTERM into a SIGINT to this process, so
// the app framework's SIGINT-based graceful shutdown fires for the SIGTERM
// that systemd/Kubernetes/docker send on stop. Idempotent enough for a single
// server process; called once before app.Run.
func bridgeSIGTERM() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)

	go func() {
		<-ch

		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(os.Interrupt)
		}
	}()
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
