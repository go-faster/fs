package main

import (
	"os"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/auth"
)

// Environment variables for the bootstrap root credential.
const (
	envRootAccessKey = "FS_ROOT_ACCESS_KEY"
	envRootSecretKey = "FS_ROOT_SECRET_KEY" //nolint:gosec // Env var name, not a credential.
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

		keys = append(keys, auth.Key{AccessKey: k.AccessKey, SecretKey: k.SecretKey, Grants: grants})
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
