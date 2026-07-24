package main

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/go-faster/fs/adminapi"
	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminhandler"
)

// buildMeta is version metadata extracted from the build.
type buildMeta struct {
	Version string
	Commit  string
}

// buildInfo reports the module version and VCS revision embedded by the Go
// toolchain, falling back to "devel"/"unknown" when unavailable.
func buildInfo() (buildMeta, bool) {
	meta := buildMeta{Version: "devel", Commit: "unknown"}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return meta, false
	}

	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		meta.Version = info.Main.Version
	}

	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			meta.Commit = s.Value
		}
	}

	return meta, true
}

// resolveAdminKeysFile returns the path where runtime-created access keys are
// persisted: the configured path, or <root>/.access-keys.json by default.
func resolveAdminKeysFile(cfg Config, absRoot string) string {
	if cfg.Admin.KeysFile != "" {
		return cfg.Admin.KeysFile
	}

	return filepath.Join(absRoot, DefaultAdminKeysFile)
}

// runAdminServer serves the admin API and its embedded web dashboard on a
// separate listener until ctx is canceled. It requires a bearer token on every
// API request. It returns an error only on a fatal serve failure.
func runAdminServer(ctx context.Context, lg *zap.Logger, t *app.Telemetry, cfg AdminConfig, mgr *auth.Manager, authEnabled bool, start time.Time, rebalance adminhandler.RebalanceControl, clusterStatus adminhandler.ClusterStatusSource, rel *reloader) error {
	addr := cfg.Addr
	if addr == "" {
		addr = DefaultAdminAddr
	}

	token := cfg.Token
	if env := os.Getenv(envAdminToken); env != "" {
		token = env
	}

	if token == "" {
		return errors.Errorf("admin API is enabled but no token is set: set admin.token or %s", envAdminToken)
	}

	build, _ := buildInfo()

	opts := adminhandler.Options{
		Manager:       mgr,
		Build:         adminhandler.BuildInfo{Version: build.Version, Commit: build.Commit},
		AuthEnabled:   authEnabled,
		StartTime:     start,
		Rebalance:     rebalance,
		ClusterStatus: clusterStatus,
	}

	// Set the reload interface only when there is a reloader: a nil *reloader
	// stored in the interface would read as non-nil and defeat the endpoint's
	// "nothing to reload" guard.
	if rel != nil {
		opts.Reloader = rel
		opts.ConfigRevision = rel.CurrentRevision
	}

	handler := adminhandler.NewAdminAPI(opts)

	s, err := adminapi.NewServer(handler,
		adminapi.WithAttributes(attribute.String("fs.api", "admin")),
		adminapi.WithTracerProvider(t.TracerProvider()),
		adminapi.WithMeterProvider(t.MeterProvider()),
	)
	if err != nil {
		return errors.Wrap(err, "create admin server")
	}

	// The UI middleware serves the SPA for non-/api paths and forwards /api/ to
	// the ogen server; the bearer guard protects the API only (static assets and
	// the SPA shell load without a token so the login page can render).
	root := adminhandler.UIMiddleware()(bearerAuth(token, s))

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return t.BaseContext() },
	}

	lg.Info("Starting admin server", zap.String("addr", addr))

	go func() { //nolint:gosec // Detached shutdown context is intentional: ctx is already canceled here.
		<-ctx.Done()

		// Shutdown needs a fresh context: ctx is already canceled here.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Wrap(err, "admin listen and serve")
	}

	return nil
}

// bearerAuth wraps h so that only /api/ requests carrying the expected bearer
// token are allowed through; other paths pass unauthenticated (static SPA).
func bearerAuth(token string, h http.Handler) http.Handler {
	want := []byte("Bearer " + token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error_message":"unauthorized"}`))

			return
		}

		h.ServeHTTP(w, r)
	})
}
