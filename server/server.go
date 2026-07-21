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
	"crypto/tls"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/cors"
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
	DefaultReadyPath    = "/ready"
)

// HandlerOption configures the handler built by NewHandler.
type HandlerOption func(*handlerOptions)

type handlerOptions struct {
	opts []handler.Option
}

// WithAuth enables SigV4 authentication and grant-based authorization on the
// handler. Without it the handler serves anonymously (the library default).
func WithAuth(store *auth.Store) HandlerOption {
	return func(o *handlerOptions) {
		o.opts = append(o.opts, handler.WithAuthenticator(store))
	}
}

// WithCORS enables per-bucket CORS (OPTIONS preflight + response headers).
func WithCORS(cfg cors.Config) HandlerOption {
	return func(o *handlerOptions) {
		o.opts = append(o.opts, handler.WithCORS(cfg))
	}
}

// NewHandler returns the S3-compatible http.Handler for a storage backend,
// wiring the validation layer and the request router. Mount it into your own
// http.Server or mux to embed the S3 API. Options enable authentication and
// CORS.
func NewHandler(store fs.Storage, opts ...HandlerOption) http.Handler {
	var o handlerOptions
	for _, opt := range opts {
		opt(&o)
	}

	return handler.New(service.New(store), o.opts...)
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

	// HealthPath is the path serving a plaintext "OK" liveness check. Defaults to
	// DefaultHealthPath ("/health"). Set to "-" to disable the health endpoint.
	HealthPath string

	// ReadyPath is the path serving a readiness check. Defaults to
	// DefaultReadyPath ("/ready"). Set to "-" to disable it. Unlike health
	// (liveness: the process is up), readiness reports whether the server can
	// actually serve — see Ready.
	ReadyPath string

	// Ready is the readiness probe. When nil, the server is ready as soon as it
	// is serving. When set, /ready runs it per request: a nil result is 200, a
	// non-nil result is 503 with the error message (e.g. storage unreachable).
	Ready func(context.Context) error

	// Buckets are created (if absent) before the server begins serving in
	// ListenAndServe / Serve.
	Buckets []string

	// Auth, if set, enables SigV4 authentication and grant-based authorization.
	// Its snapshot can be hot-reloaded via (*auth.Store).Set. Nil serves
	// anonymously.
	Auth *auth.Store

	// CORS, if non-empty, enables per-bucket CORS (preflight + headers).
	CORS cors.Config

	// TLS, if set, serves HTTPS with hot-reloadable certificates.
	TLS *TLSConfig

	// WrapHandler, if set, wraps the composed handler (health endpoint + S3
	// router) before it is served. This is the injection point for
	// observability or middleware, e.g. otelhttp.NewHandler or request logging.
	WrapHandler func(http.Handler) http.Handler
}

// TLSConfig configures TLS termination with certificates reloaded from disk.
type TLSConfig struct {
	// CertFile and KeyFile are the PEM certificate and private key paths.
	CertFile string
	KeyFile  string
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

	if c.ReadyPath == "" {
		c.ReadyPath = DefaultReadyPath
	}
}

// Server is an embeddable S3-compatible HTTP server with health checks,
// timeouts and graceful shutdown.
type Server struct {
	cfg     Config
	handler http.Handler
	http    *http.Server
	certs   *certReloader
}

// certReloader loads a TLS keypair from disk and caches it behind an atomic
// pointer, so ReloadCertificate swaps the certificate for new connections
// without dropping the listener.
type certReloader struct {
	certFile string
	keyFile  string
	cert     atomic.Pointer[tls.Certificate]
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	c := &certReloader{certFile: certFile, keyFile: keyFile}
	if err := c.Reload(); err != nil {
		return nil, err
	}

	return c, nil
}

// Reload re-reads the certificate and key from disk.
func (c *certReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return errors.Wrap(err, "load tls keypair")
	}

	c.cert.Store(&cert)

	return nil
}

func (c *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.cert.Load(), nil
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

	if cfg.TLS != nil {
		certs, err := newCertReloader(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, err
		}

		s.certs = certs
		s.http.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: certs.getCertificate,
		}
	}

	return s, nil
}

func (s *Server) buildHandler() http.Handler {
	var opts []HandlerOption
	if s.cfg.Auth != nil {
		opts = append(opts, WithAuth(s.cfg.Auth))
	}

	if len(s.cfg.CORS.Buckets) > 0 || len(s.cfg.CORS.Default) > 0 {
		opts = append(opts, WithCORS(s.cfg.CORS))
	}

	mux := http.NewServeMux()
	mux.Handle("/", NewHandler(s.cfg.Storage, opts...))

	if s.cfg.HealthPath != "" && s.cfg.HealthPath != "-" {
		mux.HandleFunc(s.cfg.HealthPath, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		})
	}

	if s.cfg.ReadyPath != "" && s.cfg.ReadyPath != "-" && s.cfg.ReadyPath != s.cfg.HealthPath {
		mux.HandleFunc(s.cfg.ReadyPath, s.readyHandler)
	}

	var h http.Handler = mux
	if s.cfg.WrapHandler != nil {
		h = s.cfg.WrapHandler(h)
	}

	return h
}

// readyHandler runs the readiness probe: 200 when ready, 503 with the error
// message otherwise.
func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Ready != nil {
		if err := s.cfg.Ready(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("NOT READY: " + err.Error()))

			return
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("READY"))
}

// Handler returns the composed http.Handler (S3 router, optional health and
// readiness endpoints and Config.WrapHandler). It can be mounted directly
// without using ListenAndServe.
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
		serve := s.http.Serve
		if s.certs != nil {
			// Certificates come from the reloader's GetCertificate callback.
			serve = func(l net.Listener) error { return s.http.ServeTLS(l, "", "") }
		}

		if err := serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// ReloadCertificate re-reads the TLS certificate and key from disk, applying
// them to new connections without interrupting the listener. It is a no-op when
// TLS is not configured.
func (s *Server) ReloadCertificate() error {
	if s.certs == nil {
		return nil
	}

	return s.certs.Reload()
}
