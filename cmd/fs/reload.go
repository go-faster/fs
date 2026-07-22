package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/server"
)

// handleReload re-applies hot-reloadable configuration on SIGHUP until ctx is
// canceled: it re-reads the config file and updates credentials/grants
// (in-place, via the atomic auth store) and the TLS certificate. It cannot
// toggle auth or TLS on/off at runtime — those change the request pipeline — so
// a reload only refreshes an already-enabled store or certificate.
func handleReload(ctx context.Context, lg *zap.Logger, configPath string, insecureNoAuth bool, authManager *auth.Manager, srv *server.Server) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)

	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			reload(lg, configPath, insecureNoAuth, authManager, srv)
		}
	}
}

// reload performs a single reload cycle. Each part is independent: a failure in
// one is logged and does not abort the others.
func reload(lg *zap.Logger, configPath string, insecureNoAuth bool, authManager *auth.Manager, srv *server.Server) {
	lg.Info("Reloading configuration (SIGHUP)")

	cfg, err := LoadConfig(configPath)
	if err != nil {
		lg.Error("Reload: failed to read config; keeping current", zap.Error(err))
		return
	}

	if authManager != nil {
		if ac, enabled, err := buildAuthConfig(cfg, insecureNoAuth); err != nil {
			lg.Error("Reload: credentials unchanged (invalid auth config)", zap.Error(err))
		} else if enabled {
			// Reload refreshes the config-defined credentials while preserving
			// keys created at runtime through the admin API.
			if err := authManager.Reload(ac); err != nil {
				lg.Error("Reload: credentials unchanged", zap.Error(err))
			} else {
				lg.Info("Reload: credentials updated", zap.Int("keys", len(ac.Keys)))
			}
		}
	}

	if cfg.Server.TLS.CertFile != "" && cfg.Server.TLS.KeyFile != "" {
		if err := srv.ReloadCertificate(); err != nil {
			lg.Error("Reload: TLS certificate unchanged", zap.Error(err))
		} else {
			lg.Info("Reload: TLS certificate reloaded")
		}
	}
}
