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

// buildAuthStore builds the auth store from configuration and environment.
// Authentication is ON by default; it is disabled only when insecureNoAuth is
// set (the --insecure-no-auth flag) or cfg.Auth.Disabled is true, in which case
// it returns (nil, nil) and the server serves anonymously.
//
// When enabled, credentials come from FS_ROOT_ACCESS_KEY/FS_ROOT_SECRET_KEY (a
// root credential with admin over all buckets) and cfg.Auth.Keys. Enabling auth
// with no credentials is an error, so a server never silently accepts nothing.
func buildAuthStore(cfg Config, insecureNoAuth bool) (*auth.Store, error) {
	if insecureNoAuth || cfg.Auth.Disabled {
		return nil, nil
	}

	var keys []auth.Key

	if ak := os.Getenv(envRootAccessKey); ak != "" {
		sk := os.Getenv(envRootSecretKey)
		if sk == "" {
			return nil, errors.Errorf("%s is set but %s is empty", envRootAccessKey, envRootSecretKey)
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
			return nil, errors.Wrapf(err, "key %q", k.AccessKey)
		}

		keys = append(keys, auth.Key{AccessKey: k.AccessKey, SecretKey: k.SecretKey, Grants: grants})
	}

	if len(keys) == 0 {
		return nil, errors.Errorf("authentication is enabled but no credentials are configured: set %s and %s, "+
			"add auth.keys to the config file, or pass --insecure-no-auth to serve anonymously",
			envRootAccessKey, envRootSecretKey)
	}

	store, err := auth.NewStore(auth.Config{Keys: keys, PublicReadBuckets: cfg.Auth.PublicReadBuckets})
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
