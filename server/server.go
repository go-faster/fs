// Package server provides an embeddable S3-compatible HTTP server.
//
// It exposes the go-faster/fs S3 implementation as a library: construct a
// storage backend (for example github.com/go-faster/fs/storagefs) and either
// build a bare http.Handler to mount into your own server, or use the Server
// type for a turnkey HTTP server with health checks, timeouts and graceful
// shutdown.
//
// The package deliberately does not pull in any observability stack. Wrap the
// handler yourself (for example with otelhttp) via Config.WrapHandler or by
// wrapping the result of NewHandler.
package server

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/go-faster/errors"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
)

// Default server configuration values.
const (
	DefaultAddr         = ":8080"
	DefaultReadTimeout  = 30 * time.Second
	DefaultWriteTimeout = 30 * time.Second
	DefaultIdleTimeout  = 120 * time.Second
	DefaultHealthPath   = "/health"
)

// NewService wraps a storage backend with the request-validation layer,
// returning the fs.Service used by the S3 handler.
func NewService(store fs.Storage) fs.Service {
	return service.New(store)
}

// NewHandler returns the S3-compatible http.Handler for a storage backend,
// wiring the validation service and the request router. Mount it into your own
// http.Server or mux to embed the S3 API.
func NewHandler(store fs.Storage) http.Handler {
	return handler.New(service.New(store))
}

// HandlerFromService returns the S3-compatible http.Handler for an
// already-constructed service. Use this to wrap or replace the default
// validation layer.
func HandlerFromService(svc fs.Service) http.Handler {
	return handler.New(svc)
}

// Config configures a Server.
type Config struct {
	// Storage is the backend used to serve S3 operations. Required.
	Storage fs.Storage

	// Addr is the TCP address to listen on. Defaults to DefaultAddr (":8080").
	Addr string

	// ReadTimeout, WriteTimeout and IdleTimeout configure the underlying
	// http.Server. Zero values fall back to the Default* constants.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// HealthPath is the path serving a plaintext "OK" health check. Defaults to
	// DefaultHealthPath ("/health"). Set to "-" to disable the health endpoint.
	HealthPath string

	// Buckets are created (if absent) before the server begins serving in
	// ListenAndServe / Serve.
	Buckets []string

	// WrapHandler, if set, wraps the composed handler (health endpoint + S3
	// router) before it is served. This is the injection point for
	// observability or middleware, e.g. otelhttp.NewHandler or request logging.
	WrapHandler func(http.Handler) http.Handler
}

func (c *Config) setDefaults() {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = DefaultReadTimeout
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.HealthPath == "" {
		c.HealthPath = DefaultHealthPath
	}
}

// Server is an embeddable S3-compatible HTTP server with health checks,
// timeouts and graceful shutdown.
type Server struct {
	cfg     Config
	handler http.Handler
	http    *http.Server
}

// New builds a Server from cfg. Storage is required. Configuration defaults are
// applied for any zero-valued fields.
func New(cfg Config) (*Server, error) {
	if cfg.Storage == nil {
		return nil, errors.New("server: Config.Storage is required")
	}

	cfg.setDefaults()

	s := &Server{cfg: cfg}
	s.handler = s.buildHandler()
	s.http = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	return s, nil
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", NewHandler(s.cfg.Storage))

	if s.cfg.HealthPath != "" && s.cfg.HealthPath != "-" {
		mux.HandleFunc(s.cfg.HealthPath, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		})
	}

	var h http.Handler = mux
	if s.cfg.WrapHandler != nil {
		h = s.cfg.WrapHandler(h)
	}

	return h
}

// Handler returns the composed http.Handler (S3 router, optional health
// endpoint and Config.WrapHandler). It can be mounted directly without using
// ListenAndServe.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// HTTPServer returns the underlying *http.Server, allowing callers to set
// advanced fields (ConnContext, BaseContext, ErrorLog, TLSConfig, ...) before
// calling ListenAndServe or Serve. The Handler, Addr and timeout fields are
// managed by New and should not be replaced.
func (s *Server) HTTPServer() *http.Server {
	return s.http
}

// ensureBuckets pre-creates the configured buckets. It is idempotent: buckets
// that already exist are left untouched, so a restart against existing storage
// does not fail regardless of backend.
func (s *Server) ensureBuckets(ctx context.Context) error {
	for _, bucket := range s.cfg.Buckets {
		exists, err := s.cfg.Storage.BucketExists(ctx, bucket)
		if err != nil {
			return errors.Wrapf(err, "check bucket %q", bucket)
		}
		if exists {
			continue
		}

		if err := s.cfg.Storage.CreateBucket(ctx, bucket); err != nil {
			return errors.Wrapf(err, "create bucket %q", bucket)
		}
	}

	return nil
}

// ListenAndServe pre-creates configured buckets, listens on Config.Addr and
// serves until ctx is canceled, then performs a graceful shutdown. It returns
// nil on a clean shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return errors.Wrap(err, "listen")
	}

	return s.Serve(ctx, ln)
}

// Serve pre-creates configured buckets and serves on ln until ctx is canceled,
// then performs a graceful shutdown. It returns nil on a clean shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := s.ensureBuckets(ctx); err != nil {
		return err
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.Wrap(err, "serve")
		}

		return nil
	})
	g.Go(func() error {
		<-gCtx.Done()

		// Graceful shutdown using a context detached from the (already canceled)
		// serving context so in-flight requests can drain.
		return s.Shutdown(context.WithoutCancel(gCtx))
	})

	return g.Wait()
}

// Shutdown gracefully shuts down the underlying HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
