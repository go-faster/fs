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
	"github.com/go-faster/fs/internal/adminhandler"
)

// buildMeta is version metadata extracted from the build.
type buildMeta struct {
	Version string
	Commit  string
}

// buildInfo reports the module version and VCS revision embedded by the Go
// toolchain, falling back to "devel"/"unknown" when unavailable.
func buildInfo() buildMeta {
	meta := buildMeta{Version: "devel", Commit: "unknown"}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return meta
	}

	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		meta.Version = info.Main.Version
	}

	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			meta.Commit = s.Value
		}
	}

	return meta
}

// resolveAdminKeysFile returns the path where runtime-created access keys are
// persisted: the configured path, or <root>/.access-keys.json by default.
func resolveAdminKeysFile(cfg Config, absRoot string) string {
	if cfg.Admin.KeysFile != "" {
		return cfg.Admin.KeysFile
	}

	return filepath.Join(absRoot, DefaultAdminKeysFile)
}

// adminServerConfig is what an admin listener serves: the listener settings,
// the credential store, and the cluster control surfaces — all of the latter
// nil outside cluster mode, where their endpoints report "disabled".
type adminServerConfig struct {
	Admin       AdminConfig
	Credentials adminhandler.CredentialManager
	// AuthEnabled reports whether the S3 server enforces SigV4; false on the
	// headless admin, which serves no S3.
	AuthEnabled bool
	StartTime   time.Time

	Rebalance            adminhandler.RebalanceControl
	ClusterStatus        adminhandler.ClusterStatusSource
	Migrations           adminhandler.MigrationControl
	BucketSchemes        adminhandler.BucketSchemeStore
	DiskWeights          adminhandler.DiskWeightStore
	BucketUsage          adminhandler.BucketUsageSource
	ClusterDefaultScheme string
	// Reloader applies hot-reloadable config; nil where there is none.
	Reloader *reloader
}

// runAdminServer serves the admin API and its embedded web dashboard on a
// separate listener until ctx is canceled. It requires a bearer token on every
// API request. It returns an error only on a fatal serve failure.
func runAdminServer(ctx context.Context, lg *zap.Logger, t *app.Telemetry, cfg adminServerConfig) error {
	addr := cfg.Admin.Addr
	if addr == "" {
		addr = DefaultAdminAddr
	}

	token := cfg.Admin.Token
	if env := os.Getenv(envAdminToken); env != "" {
		token = env
	}

	if token == "" {
		return errors.Errorf("admin API is enabled but no token is set: set admin.token or %s", envAdminToken)
	}

	build := buildInfo()

	opts := adminhandler.Options{
		Manager:              cfg.Credentials,
		Build:                adminhandler.BuildInfo{Version: build.Version, Commit: build.Commit},
		AuthEnabled:          cfg.AuthEnabled,
		StartTime:            cfg.StartTime,
		Rebalance:            cfg.Rebalance,
		ClusterStatus:        cfg.ClusterStatus,
		Migrations:           cfg.Migrations,
		BucketSchemes:        cfg.BucketSchemes,
		DiskWeights:          cfg.DiskWeights,
		BucketUsage:          cfg.BucketUsage,
		ClusterDefaultScheme: cfg.ClusterDefaultScheme,
	}

	// Cluster-wide credential stores also manage the public-read bucket list;
	// the file-backed manager does not, leaving those endpoints at 501.
	if prs, ok := cfg.Credentials.(adminhandler.PublicReadStore); ok {
		opts.PublicRead = prs
	}

	// Set the reload interface only when there is a reloader: a nil *reloader
	// stored in the interface would read as non-nil and defeat the endpoint's
	// "nothing to reload" guard.
	if rel := cfg.Reloader; rel != nil {
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
