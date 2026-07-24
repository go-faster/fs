package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/go-faster/errors"
	"go.uber.org/zap"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/server"
)

// reloader re-applies the hot-reloadable configuration — the config-defined
// credentials/grants and the TLS certificate — from the config file on demand.
// It backs both the SIGHUP handler and the admin reload endpoint, and it
// remembers the config revision currently in effect so the admin API can
// report it. It cannot toggle auth or TLS on/off at runtime (those change the
// request pipeline), so it only refreshes an already-enabled store or
// certificate.
type reloader struct {
	lg             *zap.Logger
	configPath     string
	insecureNoAuth bool
	authManager    *auth.Manager
	srv            *server.Server

	// mu guards revision, which changes on every reload.
	mu       sync.RWMutex
	revision string
}

var _ adminhandler.Reloader = (*reloader)(nil)

// newReloader builds a reloader and records the config revision at startup, so
// the admin API reports the loaded revision before any reload has run.
func newReloader(lg *zap.Logger, configPath string, insecureNoAuth bool, authManager *auth.Manager, srv *server.Server) *reloader {
	r := &reloader{
		lg:             lg,
		configPath:     configPath,
		insecureNoAuth: insecureNoAuth,
		authManager:    authManager,
		srv:            srv,
	}

	if cfg, err := LoadConfig(configPath); err == nil {
		r.revision = cfg.Revision
	}

	return r
}

// CurrentRevision returns the config revision currently in effect.
func (r *reloader) CurrentRevision() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.revision
}

// Reload re-reads the config file and applies the hot-reloadable parts,
// returning what it changed and the revision now in effect. A failure to read
// the config is returned as an error (nothing is changed); a part that fails to
// apply is also an error, so a caller learns the reload did not fully land.
func (r *reloader) Reload(_ context.Context) (adminhandler.ReloadResult, error) {
	cfg, err := LoadConfig(r.configPath)
	if err != nil {
		return adminhandler.ReloadResult{}, errors.Wrap(err, "read config")
	}

	var reloaded []string

	if r.authManager != nil {
		ac, enabled, err := buildAuthConfig(cfg, r.insecureNoAuth)
		if err != nil {
			return adminhandler.ReloadResult{}, errors.Wrap(err, "credentials unchanged (invalid auth config)")
		}

		if enabled {
			// Reload refreshes the config-defined credentials while preserving
			// keys created at runtime through the admin API.
			if err := r.authManager.Reload(ac); err != nil {
				return adminhandler.ReloadResult{}, errors.Wrap(err, "credentials")
			}

			reloaded = append(reloaded, "credentials")
		}
	}

	if cfg.Server.TLS.CertFile != "" && cfg.Server.TLS.KeyFile != "" {
		if err := r.srv.ReloadCertificate(); err != nil {
			return adminhandler.ReloadResult{}, errors.Wrap(err, "TLS certificate")
		}

		reloaded = append(reloaded, "tls")
	}

	r.mu.Lock()
	r.revision = cfg.Revision
	r.mu.Unlock()

	return adminhandler.ReloadResult{Reloaded: reloaded, ConfigRevision: cfg.Revision}, nil
}

// handleReload re-applies the hot-reloadable configuration on SIGHUP until ctx
// is canceled, logging the outcome. The admin reload endpoint drives the same
// reloader; a reload from either path is equivalent.
func handleReload(ctx context.Context, r *reloader) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)

	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			r.lg.Info("Reloading configuration (SIGHUP)")

			res, err := r.Reload(ctx)
			if err != nil {
				r.lg.Error("Reload failed; keeping current configuration", zap.Error(err))

				continue
			}

			r.lg.Info("Reload applied",
				zap.Strings("reloaded", res.Reloaded),
				zap.String("revision", res.ConfigRevision),
			)
		}
	}
}
