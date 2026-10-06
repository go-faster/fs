package main

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/go-faster/fs/auth"
)

// Environment variables for the bootstrap root credential.
const (
	envRootAccessKey = "FS_ROOT_ACCESS_KEY"
	envRootSecretKey = "FS_ROOT_SECRET_KEY" //nolint:gosec // Env var name, not a credential.
	envAdminToken    = "FS_ADMIN_TOKEN"     //nolint:gosec // Env var name, not a credential.
)

// Permission names accepted in grant configuration.
const (
	permRead  = "read"
	permWrite = "write"
	permAdmin = "admin"
)

// buildAuthConfig resolves the effective auth configuration from config and
// environment. enabled is false when authentication is off (insecureNoAuth or
// cfg.Auth.Disabled), in which case the returned auth.Config is empty.
//
// When enabled, credentials come from FS_ROOT_ACCESS_KEY/FS_ROOT_SECRET_KEY (a
// root credential with admin over all buckets) and cfg.Auth.Keys. Enabling auth
// with no credentials is an error, so a server never silently accepts nothing.
func buildAuthConfig(cfg Config, insecureNoAuth bool) (ac auth.Config, enabled bool, err error) {
	if insecureNoAuth || cfg.Auth.Disabled {
		return auth.Config{}, false, nil
	}

	var keys []auth.Key

	if ak := os.Getenv(envRootAccessKey); ak != "" {
		sk := os.Getenv(envRootSecretKey)
		if sk == "" {
			return auth.Config{}, true, errors.Errorf("%s is set but %s is empty", envRootAccessKey, envRootSecretKey)
		}

		keys = append(keys, auth.Key{
			AccessKey: ak,
			SecretKey: sk,
			Grants:    []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
		})
	}

	for _, k := range cfg.Auth.Keys {
		grants, err := parseGrants(k.Grants)
		if err != nil {
			return auth.Config{}, true, errors.Wrapf(err, "key %q", k.AccessKey)
		}

		keys = append(keys, auth.Key{
			AccessKey:   k.AccessKey,
			SecretKey:   k.SecretKey,
			UserID:      k.UserID,
			DisplayName: k.DisplayName,
			Grants:      grants,
		})
	}

	if len(keys) == 0 {
		return auth.Config{}, true, errors.Errorf("authentication is enabled but no credentials are configured: set %s and %s, "+
			"add auth.keys to the config file, or pass --insecure-no-auth to serve anonymously",
			envRootAccessKey, envRootSecretKey)
	}

	return auth.Config{Keys: keys, PublicReadBuckets: cfg.Auth.PublicReadBuckets}, true, nil
}

// buildAuthStore builds the auth store from configuration and environment,
// returning (nil, nil) when authentication is disabled.
func buildAuthStore(cfg Config, insecureNoAuth bool) (*auth.Store, error) {
	ac, enabled, err := buildAuthConfig(cfg, insecureNoAuth)
	if err != nil {
		return nil, err
	}

	if !enabled {
		return nil, nil
	}

	store, err := auth.NewStore(ac)
	if err != nil {
		return nil, errors.Wrap(err, "build auth store")
	}

	return store, nil
}

// buildAuthManager builds the auth manager from configuration and environment,
// returning (nil, nil) when authentication is disabled. backend keeps the
// credentials created through the admin API: the engine, which replicates
// them to every node.
func buildAuthManager(cfg Config, insecureNoAuth bool, backend auth.Backend) (*auth.Manager, error) {
	ac, enabled, err := buildAuthConfig(cfg, insecureNoAuth)
	if err != nil {
		return nil, err
	}

	if !enabled {
		return nil, nil
	}

	mgr, err := auth.NewManager(ac, backend)
	if err != nil {
		return nil, errors.Wrap(err, "build auth manager")
	}

	return mgr, nil
}

// parseGrants converts config grants to auth grants, defaulting an unset
// bucket pattern to "*".
func parseGrants(in []GrantConfig) ([]auth.Grant, error) {
	grants := make([]auth.Grant, len(in))

	for i, g := range in {
		perm, err := parsePermission(g.Permission)
		if err != nil {
			return nil, err
		}

		pattern := g.Bucket
		if pattern == "" {
			pattern = "*"
		}

		grants[i] = auth.Grant{Pattern: pattern, Permission: perm}
	}

	return grants, nil
}

func parsePermission(s string) (auth.Permission, error) {
	switch s {
	case permRead:
		return auth.Read, nil
	case permWrite:
		return auth.Write, nil
	case permAdmin:
		return auth.Admin, nil
	default:
		return 0, errors.Errorf("invalid permission %q (want read, write or admin)", s)
	}
}

// keyRefreshInterval is how soon a key created or deleted through another node
// takes effect on this one.
const keyRefreshInterval = 5 * time.Second

// keyRefresh pulls the replicated runtime credentials into the auth manager,
// and records what an operator needs to see it fall behind.
type keyRefresh struct {
	mgr *auth.Manager
	lg  *zap.Logger

	// lastOK is when a refresh last succeeded, in Unix nanoseconds; zero until
	// one has.
	lastOK atomic.Int64
	start  time.Time
}

// run refreshes every keyRefreshInterval until ctx ends. A failure is logged
// when refreshing starts failing and when it recovers, not every interval: a
// cluster without a layout yet fails each one, and says so once.
func (k *keyRefresh) run(ctx context.Context) {
	k.start = time.Now()

	tick := time.NewTicker(keyRefreshInterval)
	defer tick.Stop()

	failing := false

	for {
		err := k.mgr.Refresh(ctx)

		switch {
		case err == nil:
			k.lastOK.Store(time.Now().UnixNano())

			if failing {
				k.lg.Info("Access keys refreshed again")
			}

			failing = false
		case ctx.Err() != nil:
			return
		case !failing:
			k.lg.Warn("Access keys not refreshed; keys created through other nodes are not accepted here yet",
				zap.Error(err))

			failing = true
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// age is how long since a refresh last succeeded — since start, before one
// has.
func (k *keyRefresh) age() time.Duration {
	if t := k.lastOK.Load(); t != 0 {
		return time.Since(time.Unix(0, t))
	}

	return time.Since(k.start)
}

// register exports the refresh's state on the meter provider.
func (k *keyRefresh) register(mp metric.MeterProvider) error {
	meter := mp.Meter("github.com/go-faster/fs/auth")

	age, err := meter.Int64ObservableGauge("fs.auth.keys.refresh_age",
		metric.WithDescription("Seconds since this node last read the replicated access keys; "+
			"keys created through other nodes take effect here no sooner."),
		metric.WithUnit("s"))
	if err != nil {
		return errors.Wrap(err, "refresh age gauge")
	}

	managed, err := meter.Int64ObservableGauge("fs.auth.keys.managed",
		metric.WithDescription("Access keys created through the admin API that this node accepts."))
	if err != nil {
		return errors.Wrap(err, "managed keys gauge")
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(age, int64(k.age().Seconds()))

		var n int64

		for _, info := range k.mgr.List() {
			if info.Source == auth.SourceManaged {
				n++
			}
		}

		o.ObserveInt64(managed, n)

		return nil
	}, age, managed)
	if err != nil {
		return errors.Wrap(err, "register key metrics")
	}

	return nil
}
